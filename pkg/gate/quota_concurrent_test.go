// quota_concurrent_test.go —— 匿名上传日配额的**并发**语义（L5 层：配额竞争）
//
// ## 为什么立这一组
//
// 「配额竞争」的经典形态是「读已用量 → 判定 → 落库」无锁无事务 ⇒ N 个并发请求
// 各自读到同一个旧值、全部放行 ⇒ 合计可超配额。
//
// 匿名上传日配额（pkg/gate/quota.go）是**匿名面刷爆存储的主通道**（该文件头自述：
// 「此前配额只对登录用户生效，匿名分片/直传完全绕过——刷爆存储的主通道」）：
// 登录用户那条链由 app/share 的预留账本（core#169）兜底，**匿名这条链只有这里的
// 计数器** ⇒ 它就是匿名面的「读-判-记」原语。
//
// 既有覆盖的缺口（逐条核对过）：
//   - pkg/gate/quota_test.go:TestDailyQuota_Concurrent —— 50 并发**只**断言
//     `quota.mem[ip].count == 50`：① **字节维度零断言**（addBytes 累加是否丢）；
//     ② **不断言放行数**（并发下到底放行了几个，一个字都没说）；
//     ③ 只测了**内存面**，Redis 面（生产部署形态、多副本共享）**零并发覆盖**。
//   - app/share/quota_concurrent_test.go —— 站点级/用户级/目录级三个维度都验了
//     「并发不超放」，但那是 app/share 的预留账本，与本文件是**两条不同的链**
//     （登录用户 vs 匿名），不构成对本文件的覆盖。
//
// ## 本文件钉的三条不变量
//
//	① 计数不丢：N 次并发请求 ⇒ 计数器 == N（不是 N 的子集）。
//	② 字节不丢：N 次并发请求（各 addBytes 字节）⇒ 字节计数 == N×addBytes。
//	③ **不超放**：并发下**恰好** maxCount 个请求被放行，其余全部被拒（10014）。
//	   这一条是「配额竞争」的核心：计数若非原子，N 个并发请求会各自读到同一个
//	   旧值并全部放行 ⇒ 实际放行数 > maxCount ⇒ 匿名面配额形同虚设。
//
// ## 负对照（必须能变红，实跑过）
//
//   - 把 incrMem 的互斥去掉（退回裸「读-比-写」）⇒ ①③ 双红（计数丢失 + 超放）。
//   - 把 incrRedis 的原子 INCR 换成 GET+SET（读-比-写）⇒ ③ 红（Redis 面超放）。
//
// ## 为什么内存面与 Redis 面各测一遍
//
// 两者由**不同机制**保证原子性（内存面 = 互斥锁；Redis 面 = 单键原子 INCR，
// 收编到 oekit/redisx 的 Counter.IncrWithTTL）⇒ 只测一面等于把另一面的
// 原子性寄托在「看起来没问题」上。本文件两面都钉。
package gate

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/pigeonbox/contracts/errcode"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// quotaConcurrentIP 本文件统一的测试来源 IP。
const quotaConcurrentIP = "203.0.113.77"

// quotaConcurrentDay 当日键（日配额按自然日分窗，键名含日期）。
func quotaConcurrentDay() string { return time.Now().Format("20060102") }

// runConcurrentQuota n 个 goroutine 同时打 CheckAnonymousQuota，返回
// （放行数, 被拒数, 首个被拒错误）。
func runConcurrentQuota(t *testing.T, n int, addBytes int64) (passed int, rejected int, firstErr error) {
	t.Helper()
	ctx := context.Background()
	var wg sync.WaitGroup
	start := make(chan struct{})
	var passedN, rejectedN int32
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 同步起跑：避免逐个启动把并发退化成串行
			err := CheckAnonymousQuota(ctx, quotaConcurrentIP, addBytes)
			errs[i] = err
			if err == nil {
				atomic.AddInt32(&passedN, 1)
			} else {
				atomic.AddInt32(&rejectedN, 1)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	for _, e := range errs {
		if e != nil && firstErr == nil {
			firstErr = e
		}
	}
	return int(passedN), int(rejectedN), firstErr
}

// TestCheckAnonymousQuota_ConcurrentExactlyMaxCountAllowed 内存面：并发不得超放。
//
// 缺陷形态（计数非原子）：N 个并发请求各自读到同一个 used ⇒ 全部放行 ⇒
// 实际放行数 > maxCount ⇒ 「今日上传次数上限」形同虚设。
//
// 断言口径：
//   - 恰好 maxCount 个放行（不多不少）；
//   - 其余全部被拒，且错误是 typed 的 GateError + 业务码 10014（不是裸 error）；
//   - 内部计数器 == N（计数一条不丢——这是「不超放」的另一面：不能靠少计数
//     来假装没超放）。
func TestCheckAnonymousQuota_ConcurrentExactlyMaxCountAllowed(t *testing.T) {
	const maxCount = 50
	const n = 200

	withQuotaConf(t, maxCount, 0) // 只限次数，不限字节
	resetQuotaMem(t)

	passed, rejected, firstErr := runConcurrentQuota(t, n, 1)

	assert.Equal(t, maxCount, passed,
		"并发下必须**恰好**放行 %d 个（计数非原子 ⇒ 全部读到同一个 used 而全放行）", maxCount)
	assert.Equal(t, n-maxCount, rejected, "其余必须全部被拒")

	require.Error(t, firstErr, "超限请求必须给出错误")
	var gateErr *GateError
	require.ErrorAs(t, firstErr, &gateErr, "必须是 typed GateError（handler 据此透传业务码）")
	assert.Equal(t, errcode.CodeAnonymousQuota, gateErr.ErrCode(),
		"超限业务码必须是 10014（不得退化成裸 500）")

	quota.mu.Lock()
	defer quota.mu.Unlock()
	require.NotNil(t, quota.mem[quotaConcurrentIP], "计数必须落在本 IP 的桶里")
	assert.Equal(t, int64(n), quota.mem[quotaConcurrentIP].count,
		"计数器必须等于请求数（计数一条不丢；少计数会让超放看不出来）")
}

// TestCheckAnonymousQuota_ConcurrentBytesNotLost 内存面：并发下字节计数不丢。
//
// 缺陷形态（字节累加非原子）：每字节用「读 → 加 → 写」⇒ 并发下丢字节 ⇒
// 「今日上传流量上限」被绕过（用户可以上传远超上限的流量）。
//
// 断言口径：N 次并发请求各 addBytes 字节 ⇒ 字节计数 == N×addBytes。
func TestCheckAnonymousQuota_ConcurrentBytesNotLost(t *testing.T) {
	const n = 200
	const each = 7

	withQuotaConf(t, 0, 1<<30) // 字节上限放到 1GB（本用例只关心「记准」，不触发拒绝）
	resetQuotaMem(t)

	passed, rejected, _ := runConcurrentQuota(t, n, each)
	assert.Equal(t, n, passed, "字节上限未触发时全部放行")
	assert.Equal(t, 0, rejected)

	quota.mu.Lock()
	defer quota.mu.Unlock()
	require.NotNil(t, quota.mem[quotaConcurrentIP])
	assert.Equal(t, int64(n*each), quota.mem[quotaConcurrentIP].bytes,
		"并发下字节计数不得丢失（丢了 ⇒ 日流量上限被绕过）")
	assert.Equal(t, int64(n), quota.mem[quotaConcurrentIP].count)
}

// TestCheckAnonymousQuota_ConcurrentRedisExactlyMaxCountAllowed Redis 面：并发不得超放。
//
// 为什么要单独测 Redis 面：生产部署形态是**多副本共享 Redis**（redis.host 非空），
// 内存面只是单机兜底 ⇒ 两面原子性由不同机制保证（互斥锁 vs 单键原子 INCR）。
// 本用例把 Redis 面的「恰好放行 maxCount 个」钉住（负对照：把 INCR 换成
// GET+SET 读-比-写 ⇒ 放行数超限 ⇒ 红）。
func TestCheckAnonymousQuota_ConcurrentRedisExactlyMaxCountAllowed(t *testing.T) {
	const maxCount = 50
	const n = 200
	const each = 7

	s := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	SetQuotaRedis(client)

	withQuotaConf(t, maxCount, 1<<30)
	t.Cleanup(func() { SetQuotaRedis(nil) })

	passed, rejected, firstErr := runConcurrentQuota(t, n, each)
	assert.Equal(t, maxCount, passed,
		"Redis 面并发下必须**恰好**放行 %d 个（INCR 非原子 ⇒ 全放行）", maxCount)
	assert.Equal(t, n-maxCount, rejected)

	require.Error(t, firstErr)
	var gateErr *GateError
	require.ErrorAs(t, firstErr, &gateErr)
	assert.Equal(t, errcode.CodeAnonymousQuota, gateErr.ErrCode())

	ctx := context.Background()
	base := fmt.Sprintf("pb:quota:%s:%s", quotaConcurrentIP, quotaConcurrentDay())
	cnt, err := client.Get(ctx, base+":count").Int64()
	require.NoError(t, err, "次数计数键必须存在")
	assert.Equal(t, int64(n), cnt, "Redis 次数计数必须等于请求数（一条不丢）")
	byt, err := client.Get(ctx, base+":bytes").Int64()
	require.NoError(t, err, "字节计数键必须存在")
	assert.Equal(t, int64(n*each), byt, "Redis 字节计数不得丢失")
}
