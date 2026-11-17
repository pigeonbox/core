package gate

// quota_redis_fault_test.go —— 匿名日配额面的 **Redis 运行期故障注入**（L6 故障注入）。
//
// 注入什么故障：
//   miniredis Close()（等价「Redis 运行期宕机」：监听端口消失 ⇒ 连接被拒），
//   随后 Restart()（同端口同客户端复活 ⇒ 故障恢复）。miniredis v2.40 的
//   Restart 保留已写入的值（miniredis.go:280「Values will be preserved」），
//   所以「宕机前的计数」在恢复后仍在 —— 这正是「恢复后配额不丢」的判据基础。
//
// 断言什么（全部是**行为**，不是「不报错」）：
//   ① 宕机期配额面**不阻断业务**：CheckAnonymousQuota 返回 nil（fail-open）。
//      依据：pkg/gate/quota.go 的既有定位「计数基础设施故障不阻断业务（与配额
//      『运营约束』定位一致），仅告警」——配额是运营约束，不是安全边界。
//      配套断言：宕机期的流量**不落计数**（计数器不可用 ⇒ 不假装计过）。
//   ② 恢复后**配额不丢**：恢复后第一次计数从宕机前的值继续往上加（不是从 0
//      重来、也不是被清零）⇒ 宕机既没让配额「重置」也没让计数「归零」。
//   ③ 恢复后新计数**真的写进 miniredis**（证明自动切回 Redis 路径，而不是
//      粘死在某个降级态；同时证明没有偷偷回落到进程内内存计数）。
//   ④ 恢复后配额**仍然生效**：超限仍按 10014 拒（宕机没有把配额闸永久放开）。
//
// 怎么让它变红（负对照，详见交付说明的实跑记录）：
//   - 把 CheckAnonymousQuota 的 err 分支从 fail-open 改成 fail-closed
//     （返回 GateError）⇒ ①红（宕机期请求被业务拒绝）；
//   - 把 incrRedis 失败改成**回退内存计数**（降级到进程内 map 继续计数）
//     ⇒ ①的「不落计数」红、③的「写进 miniredis」红（计数落到内存去了）；
//   - 恢复后若客户端被永久降级闩锁（不再走 Redis）⇒ ③红。
//
// 用例隔离：quota 是包级单例（quota.go:48），本文件全程用既有测试辅助
// withQuotaConf/resetQuotaMem 复位（quota_test.go），避免污染同包其它用例。

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newQuotaFaultRedis 起真 miniredis + **快速失败**的 go-redis 客户端。
//
// 快速失败是硬要求：故障注入必然产生连接池重拨，若客户端按默认重试
// （MaxRetries=3 + 指数退避），单条命令要等百毫秒级，叠加 10 次重试后
// 判据会退化成时序竞态。口径照抄 pkg/middleware/ratelimit_periodlimit_test.go
// 的 newPeriodTestRedis（MaxRetries=-1 不在命令层重试；DialerRetries=1 让
// **拨号失败**也不重试——go-redis v9 的连接层默认重试 5 次、每次隔 100ms）。
func newQuotaFaultRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{
		Addr:          mr.Addr(),
		MaxRetries:    -1,
		DialerRetries: 1,
		DialTimeout:   10 * time.Millisecond,
		ReadTimeout:   2 * time.Second,
	})
	t.Cleanup(func() { _ = c.Close() })
	return mr, c
}

// quotaCountKey 当前自然日的次数计数键（与 pkg/gate/quota.go 的 incrRedis 同口径）。
func quotaCountKey(ip string) string {
	return "pb:quota:" + ip + ":" + time.Now().Format("20060102") + ":count"
}

// quotaBytesKey 当前自然日的流量计数键。
func quotaBytesKey(ip string) string {
	return "pb:quota:" + ip + ":" + time.Now().Format("20060102") + ":bytes"
}

// TestQuotaRedis_OutageDegradesAndRecovers Redis 宕机 ⇒ 降级 ⇒ 恢复的主用例。
//
// 阶段编排：
//
//	① 健康期：计数 2 次（Redis 路径，键真实存在）；
//	② 宕机期（Close）：请求放行（fail-open）且**不落计数**；
//	③ 恢复期（Restart，同端口同客户端）：计数从 2 继续到 3（配额不丢），
//	   计数键**真的写进 miniredis**（自动切回 Redis 路径），且超限仍被拒
//	   （配额闸没被宕机永久放开）。
func TestQuotaRedis_OutageDegradesAndRecovers(t *testing.T) {
	withQuotaConf(t, 5, 0) // 次数上限 5、流量不限
	mr, c := newQuotaFaultRedis(t)
	SetQuotaRedis(c)
	ctx := context.Background()
	const ip = "203.0.113.77"

	// ① 健康期：2 次计数都落进 Redis（证明走的是 Redis 路径而非内存兜底）。
	require.NoError(t, CheckAnonymousQuota(ctx, ip, 10))
	require.NoError(t, CheckAnonymousQuota(ctx, ip, 10))
	require.True(t, mr.Exists(quotaCountKey(ip)), "健康期计数键必须存在于 miniredis")
	v, err := mr.Get(quotaCountKey(ip))
	require.NoError(t, err)
	require.Equal(t, "2", v, "健康期应计数 2 次")

	// ② 宕机期：Redis 挂 ⇒ 配额面不阻断业务（fail-open，运营约束不误伤用户）。
	mr.Close()
	require.NoError(t, CheckAnonymousQuota(ctx, ip, 10),
		"Redis 宕机不得阻断业务（配额是运营约束，不是安全边界）")
	// 宕机期流量不落计数：Redis 不可用 ⇒ 计数器不可信 ⇒ 不假装计过。
	// 判据是**计数值不变**（键本身在宕机前就存在，不能拿 Exists 判）。
	// （若降级改成「回退内存计数并放行」，这条红——计数会悄悄转移到进程内 map。）
	vAfterOutage, err := mr.Get(quotaCountKey(ip))
	require.NoError(t, err)
	assert.Equal(t, "2", vAfterOutage, "宕机期不得产生计数（计数器不可用 ⇒ 不假装计过）")

	// ③ 恢复期：同端口同客户端复活 ⇒ 自动切回 Redis 路径。
	require.NoError(t, mr.Restart())
	// 恢复后第一次计数：计数键写进 miniredis，且**从宕机前的 2 继续**（配额不丢）。
	require.NoError(t, CheckAnonymousQuota(ctx, ip, 10))
	require.True(t, mr.Exists(quotaCountKey(ip)),
		"恢复后计数必须写进 miniredis（自动切回 Redis 路径，不得粘死在降级态）")
	v, err = mr.Get(quotaCountKey(ip))
	require.NoError(t, err)
	assert.Equal(t, "3", v, "恢复后计数应从 2 继续到 3（宕机前的配额不得丢失）")

	// 进程内内存计数不得被偷偷启用（否则就是「悄悄换了计数器」）。
	quota.mu.Lock()
	memCount := quota.mem[ip]
	quota.mu.Unlock()
	assert.Nil(t, memCount, "Redis 路径恢复后不得同时往进程内内存计数器写（计数口径不得分裂）")

	// ④ 恢复后配额仍然生效：补到超限 ⇒ 仍按 10014 拒（宕机没把配额闸永久放开）。
	for i := 0; i < 2; i++ {
		require.NoError(t, CheckAnonymousQuota(ctx, ip, 10))
	}
	err = CheckAnonymousQuota(ctx, ip, 10)
	require.Error(t, err, "恢复后超限必须仍被拒（配额闸不得因宕机而失效）")
	ge, ok := err.(*GateError)
	require.True(t, ok, "应为 *GateError: %v", err)
	assert.Equal(t, 10014, ge.ErrCode())
}

// TestQuotaRedis_OutageKeepsOtherIPsCounting 宕机只影响**故障期**的计数，
// 不影响别的来源 IP 的配额状态（闸是按 IP 维度的，宕机不该把所有人一起清零）。
func TestQuotaRedis_OutageKeepsOtherIPsCounting(t *testing.T) {
	withQuotaConf(t, 5, 0)
	mr, c := newQuotaFaultRedis(t)
	SetQuotaRedis(c)
	ctx := context.Background()

	require.NoError(t, CheckAnonymousQuota(ctx, "198.51.100.1", 10))
	mr.Close()
	require.NoError(t, CheckAnonymousQuota(ctx, "198.51.100.1", 10))
	require.NoError(t, mr.Restart())

	// 恢复后：该 IP 的计数从 1 继续（宕机前那 1 次没丢）。
	require.NoError(t, CheckAnonymousQuota(ctx, "198.51.100.1", 10))
	v, err := mr.Get(quotaCountKey("198.51.100.1"))
	require.NoError(t, err)
	assert.Equal(t, "2", v, "恢复后计数应从 1 继续到 2（宕机前的计数不得丢失）")

	// 别的 IP 独立计数（不被这次宕机牵连）。
	require.NoError(t, CheckAnonymousQuota(ctx, "198.51.100.2", 10))
	v2, err := mr.Get(quotaCountKey("198.51.100.2"))
	require.NoError(t, err)
	assert.Equal(t, "1", v2, "不同 IP 各自独立计数")
}

// TestQuotaRedis_OutageDoesNotCorruptByteCounter 流量维度同样受保护：
// 宕机期不落流量计数 ⇒ 恢复后流量计数从宕机前的值继续（不丢、不虚增）。
func TestQuotaRedis_OutageDoesNotCorruptByteCounter(t *testing.T) {
	withQuotaConf(t, 0, 1000) // 次数不限、流量上限 1000
	mr, c := newQuotaFaultRedis(t)
	SetQuotaRedis(c)
	ctx := context.Background()
	const ip = "203.0.113.88"

	require.NoError(t, CheckAnonymousQuota(ctx, ip, 400))
	bv, err := mr.Get(quotaBytesKey(ip))
	require.NoError(t, err)
	require.Equal(t, "400", bv)

	mr.Close()
	require.NoError(t, CheckAnonymousQuota(ctx, ip, 400))

	require.NoError(t, mr.Restart())
	require.NoError(t, CheckAnonymousQuota(ctx, ip, 100))
	bv, err = mr.Get(quotaBytesKey(ip))
	require.NoError(t, err)
	assert.Equal(t, "500", bv, "恢复后流量计数应从 400 继续到 500（宕机期既不丢也不虚增）")

	// 流量超限仍被拒（1000 上限）。
	require.NoError(t, CheckAnonymousQuota(ctx, ip, 500))
	err = CheckAnonymousQuota(ctx, ip, 1)
	require.Error(t, err, "流量超限必须被拒")
	ge, ok := err.(*GateError)
	require.True(t, ok, "应为 *GateError: %v", err)
	assert.Equal(t, 10014, ge.ErrCode())
}
