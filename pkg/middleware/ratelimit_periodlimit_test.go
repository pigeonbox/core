package middleware

// allowRedis 收编 oekit/redisx.PeriodLimit 后的行为锁（core#83 阶段 1）。
//
// 收编只把「窗口判定」（Lua 原子 INCRBY + 首次 EXPIRE + 三态返回）上收上游，
// 本仓保留的口径在这里逐条锁定：
//
//   - 三态：Allowed / HitQuota 放行（第 qps 个请求放行）、OverQuota 拒
//     （仍由 allow 的 addBlock 落短封禁——封禁双层不并入限流判定）；
//   - Unknown / err ⇒ 回退本地桶（原降级语义），PeriodLimit 缓存实例保留
//     ⇒ 恢复后自动切回 Redis，不粘死在本地；
//   - 键形状 "pb:rl:cnt:<scope>:<ip>"（窗口在 TTL，不在键后缀）；
//   - per-scope 配额热更（快照失配 ⇒ 惰性重建，同一窗口内阈值即切）。

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	redisxratelimit "github.com/oekit/redisx/ratelimit"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/pkg/metrics"
)

// redisLogDiscard 静音 go-redis 内部日志：故障注入用例（Close miniredis 制造
// Redis 故障）必然产生连接池重拨告警，属预期噪声——与上游 redisx/ratelimit
// 的测试同款处理，保持 go test 输出洁净。
type redisLogDiscard struct{}

func (redisLogDiscard) Printf(context.Context, string, ...any) {}

// mustGet 取 miniredis 键值（错误直接致命：取不到比断言错更说明环境坏了）。
func mustGet(t *testing.T, mr *miniredis.Miniredis, key string) string {
	t.Helper()
	v, err := mr.Get(key)
	require.NoError(t, err)
	return v
}

func TestMain(m *testing.M) {
	redis.SetLogger(redisLogDiscard{})
	os.Exit(m.Run())
}

// newPeriodTestRedis 起真 miniredis + 快速失败的 go-redis 客户端。
//
// 故障注入场景（Close miniredis）必须单次命令快速失败，否则慢到足以让本地桶
// 在两次 allow 之间补回令牌（rate 5/s ⇒ 200ms 一枚），把「burst 用尽应拒」的
// 判据变成时序竞态。两处缺一不可（实测逐一定位）：
//   - MaxRetries=-1：命令层不重试；
//   - DialerRetries=1：**连接层**拨号失败不重试——go-redis v9.23 的 pool 对
//     拨号失败默认重试 5 次、每次间隔 100ms（DialerRetries<=0 会被钳回 5），
//     且这条重试在 MaxRetries 语义之外 ⇒ 单命令 ~400ms。
//
// ⚠️ **ReadTimeout 刻意放宽到 2s**（原 50ms，2026-11-09 修）：
//
// 短 ReadTimeout 会把「机器慢」误判成「Redis 故障」：CPU 压力下**健康请求**
// 也会超时 ⇒ Take 返回 Unknown ⇒ 降级到本地桶；而本地桶在健康期从未被消耗
// （Redis 路径不碰它，仍是满桶）⇒ 降级瞬间额外放行一整个 burst ⇒
// 「超额应拒」类判据在压力下假红（整包跑红、单跑绿的典型形态）。
//
// 短 ReadTimeout 想防的「故障命令太慢 ⇒ 本地桶补回令牌」并不成立：Close 后是
// **拨号被拒**（connection refused），实测 0.2~0.7ms 立即失败，根本不等超时；
// 真正的等待只发生在「连不上但端口还在」的场景，本仓故障注入不制造该场景。
//
// 定位手法（可复用）：① 把实现 stash 回 HEAD 原码，在同一 CPU 压力下跑同一
// 用例——HEAD 也红 ⇒ 既有问题；② 写探针直接问判定依据（短超时探针看到
// decision.Source=local-fallback ⇒ 坐实是降级路径而非限流语义）。
func newPeriodTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
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

// newPeriodTestLimiter 装配 UseRedis=true 的限流器并注入给定客户端。
func newPeriodTestLimiter(t *testing.T, qps, burst, blockSeconds int, c *redis.Client) *RateLimiter {
	t.Helper()
	rl := NewRateLimiter(RateLimitConfig{
		GlobalQPS:    qps,
		Burst:        burst,
		Enabled:      true,
		BlockSeconds: blockSeconds,
		UseRedis:     true,
	})
	rl.SetRedis(c)
	t.Cleanup(rl.Stop)
	return rl
}

// TestRateLimiter_PeriodLimit_OverQuotaWritesBlock 收编后的主行为用例：
// Redis 路径超额被拒（OverQuota 生效）+ addBlock 照旧落「内存 + Redis」双层
// 封禁——限流与短封禁是两层语义，收编只换窗口判定、不动封禁层。
//
// 负对照：
//   - 若把 HitQuota（恰好用完配额）误当拒绝 ⇒ 第 qps 次就被拒，前 qps 次
//     放行断言红；
//   - 若封禁层被并入限流判定（OverQuota 不再 addBlock）⇒ isBlocked 与
//     Exists(pb:rl:block:…) 两条断言红；
//   - 若键形状退回旧「epoch 秒后缀键」或前缀拼错 ⇒ Exists(无后缀键) 红。
func TestRateLimiter_PeriodLimit_OverQuotaWritesBlock(t *testing.T) {
	mr, c := newPeriodTestRedis(t)
	rl := newPeriodTestLimiter(t, 3, 3, 60, c)
	ctx := context.Background()
	const ip = "203.0.113.10"

	// 前 qps 次放行（第 3 次 = HitQuota：恰好用完配额仍放行，与旧 n<=qps 同边界）
	for i := 1; i <= 3; i++ {
		assert.True(t, rl.allow(ctx, scopeGlobal, ip), "第 %d 次应放行", i)
	}
	// 第 qps+1 次起拒绝（OverQuota）
	assert.False(t, rl.allow(ctx, scopeGlobal, ip), "超额第 1 次应拒")
	assert.False(t, rl.allow(ctx, scopeGlobal, ip), "超额第 2 次应拒")

	// Redis 路径证据 + 键形状锁。计数 = 4 而非 5：第 4 次请求被 OverQuota 拒
	// 但 Lua INCRBY 先计数后判定（与旧手搓 INCR 同口径），第 5 次请求在
	// isBlocked 就被**封禁层短路**、根本没到计数层——这正是双层语义的体现
	// （封禁层先于限流层，与旧实现 isBlocked→allowRedis 的顺序一致）。
	key := redisCountKeyPrefix(scopeGlobal) + ":" + ip
	require.True(t, mr.Exists(key), "计数键 %s 不存在 ⇒ 走的不是 Redis 路径", key)
	assert.Equal(t, "4", mustGet(t, mr, key),
		"应计数 4 次（3 次放行 + 1 次被拒仍计数；第 5 次被封禁层短路）")

	// 封禁双层：内存 + Redis 各一份（addBlock 原样）
	assert.True(t, rl.isBlocked(ctx, scopeGlobal, ip), "超额后应被封禁")
	assert.True(t, mr.Exists("pb:rl:block:"+blockKey(scopeGlobal, ip)), "Redis 侧封禁键应存在")
	rl.mu.Lock()
	_, inMem := rl.blocked[blockKey(scopeGlobal, ip)]
	rl.mu.Unlock()
	assert.True(t, inMem, "内存侧封禁应存在")
}

// TestRateLimiter_PeriodLimit_HitQuotaAndWindowReset 窗口语义锁：
// 恰满配额（HitQuota）仍放行、计数键 TTL = 窗口（固定窗口由 TTL 承载）、
// 窗口过期 ⇒ 配额重置。SetTime 冻结 miniredis 虚拟时钟 ⇒ 判据不受真实
// 调度抖动影响（窗口只在 FastForward 时推进）。
//
// 负对照：
//   - 若 TTL 被每次 Take 续期（退化成滑动窗口）⇒ FastForward 后「重置后计数
//     应为 1」断言红（计数不清零）；
//   - 若窗口常量被调大（如 2s）⇒ TTL ≤ 1s 断言红；
//   - 撤 WithPeriodAlign ⇒ 1s 窗口下上游对齐/不对齐的 TTL 计算两式恰好重合
//     （period - unix%period == period），**当天无可观测差异**——该选项为
//     「窗口将来调大时保持 epoch 对齐」而接线；若届时撤掉它，窗口起点随
//     首请求漂移，本用例按窗口锚点设计的 TTL/重置判据将红。
func TestRateLimiter_PeriodLimit_HitQuotaAndWindowReset(t *testing.T) {
	mr, c := newPeriodTestRedis(t)
	rl := newPeriodTestLimiter(t, 2, 2, 0, c) // BlockSeconds=0：隔离限流语义本身
	ctx := context.Background()
	const ip = "198.51.100.7"
	key := redisCountKeyPrefix(scopeGlobal) + ":" + ip
	mr.SetTime(time.Now()) // 冻结虚拟时钟：窗口只在 FastForward 时推进

	// 第 1、2 次（= qps）放行：第 2 次 HitQuota
	assert.True(t, rl.allow(ctx, scopeGlobal, ip))
	assert.True(t, rl.allow(ctx, scopeGlobal, ip), "第 qps 次（恰满配额）仍放行")
	assert.False(t, rl.allow(ctx, scopeGlobal, ip), "第 qps+1 次拒")

	// TTL 断言：窗口由 TTL 承载且不大于窗口本身
	ttl := mr.TTL(key)
	assert.Greater(t, ttl, time.Duration(0), "计数键必须带 TTL")
	assert.LessOrEqual(t, ttl, redisWindowPeriod, "TTL 不得大于窗口")

	// FastForward 越过窗口 ⇒ 配额重置（重置后计数从 1 重新起）
	mr.FastForward(redisWindowPeriod + time.Second)
	assert.True(t, rl.allow(ctx, scopeGlobal, ip), "窗口过期后应重置放行")
	assert.Equal(t, "1", mustGet(t, mr, key), "重置后计数应从 1 重新起（旧计数已过期）")
}

// TestRateLimiter_PeriodLimit_SetRedisDegradeAndRecover 装配面降级/恢复：
// SetRedis(nil) ⇒ 整条 Redis 路径摘除，allow() 走本地桶仍能放行（不 500、
// 不粘死）；SetRedis(新客户端) ⇒ Redis 路径恢复（PeriodLimit 按 store 快照
// 失配重建，打到新端）。
//
// 负对照：
//   - 若降级口径改成 fail-open 恒放行 ⇒ 本地桶 burst 用尽后的拒绝断言红；
//   - 若 PeriodLimit 缓存不按 store 失配重建（粘在旧客户端）⇒ 恢复后的计数
//     键写进旧 miniredis ⇒ 新端 Exists 红；
//   - 若误把封禁层也随降级摘掉 ⇒ 被封禁 IP 在降级期的拒绝断言红（封禁是
//     独立一层，BlockSeconds 语义原样）。
func TestRateLimiter_PeriodLimit_SetRedisDegradeAndRecover(t *testing.T) {
	mrA, cA := newPeriodTestRedis(t)
	rl := newPeriodTestLimiter(t, 3, 3, 60, cA)
	ctx := context.Background()
	const ipA = "203.0.113.10"

	// A. Redis 健康：超额被拒 + 封禁落两层
	for i := 1; i <= 3; i++ {
		assert.True(t, rl.allow(ctx, scopeGlobal, ipA))
	}
	assert.False(t, rl.allow(ctx, scopeGlobal, ipA), "超额应拒")
	assert.True(t, rl.isBlocked(ctx, scopeGlobal, ipA))
	require.True(t, mrA.Exists(redisCountKeyPrefix(scopeGlobal)+":"+ipA), "A 阶段应走 Redis 路径")

	// B. SetRedis(nil)：Redis 路径整条摘除
	rl.SetRedis(nil)
	// 封禁中的 IP 依旧被挡（内存侧封禁仍在——双层语义不随降级消失）
	assert.False(t, rl.allow(ctx, scopeGlobal, ipA), "被封禁 IP 在降级期依旧被挡")
	// 新 IP 走本地桶：burst 内放行、burst 外拒绝
	const ipB = "203.0.113.11"
	for i := 1; i <= 3; i++ {
		assert.True(t, rl.allow(ctx, scopeGlobal, ipB), "降级期本地桶第 %d 次应放行", i)
	}
	assert.False(t, rl.allow(ctx, scopeGlobal, ipB), "降级期本地桶 burst 用尽应拒")
	assert.False(t, mrA.Exists(redisCountKeyPrefix(scopeGlobal)+":"+ipB),
		"降级期不应有 Redis 调用（旧端不应出现新 IP 的计数键）")

	// C. SetRedis(新客户端)：Redis 路径恢复，且打到新端
	mrB, cB := newPeriodTestRedis(t)
	rl.SetRedis(cB)
	const ipC = "203.0.113.12"
	for i := 1; i <= 3; i++ {
		assert.True(t, rl.allow(ctx, scopeGlobal, ipC), "恢复后第 %d 次应放行", i)
	}
	assert.False(t, rl.allow(ctx, scopeGlobal, ipC), "恢复后超额应拒")
	require.True(t, mrB.Exists(redisCountKeyPrefix(scopeGlobal)+":"+ipC), "恢复后应写进新端")
	assert.False(t, mrA.Exists(redisCountKeyPrefix(scopeGlobal)+":"+ipC), "新端流量不应写进旧端")
}

// TestRateLimiter_PeriodLimit_UnknownFallsBackAndAutoRecovers 三态降级的核心
// 行为：Redis 通信故障 ⇒ Take 返回 (Unknown, err) ⇒ 回退本地桶（原降级语义，
// 不是 fail-open 也不是 fail-closed）；同一客户端恢复（miniredis Restart 同
// 端口复活）⇒ 缓存的 PeriodLimit 实例保留，下一次 Take 自动切回 Redis 路径
// ——恢复后不永久停在本地兜底（core#83 点名的真实隐患）。
//
// 本地桶取 Burst=1：故障期的判定时序必须远小于本地桶的补币周期
// （qps=5 ⇒ 200ms 一枚），否则两次调用之间补回令牌会把「burst 用尽应拒」
// 变成时序竞态（DialTimeout=10ms 已把单次故障命令压到 ~20ms，留 10 倍余量）。
//
// 负对照：
//   - 撤 Unknown/err 回退（把 Unknown 当放行）⇒ 故障期第 2 次请求（本地桶
//     已耗尽）仍放行 ⇒ 拒绝断言红（恒放行回归）；
//   - 把 Unknown 当拒绝 ⇒ 故障期首个请求就 false ⇒ 放行断言红；
//   - 恢复后若粘死在本地桶（缓存被丢弃/降级闩锁不复位）⇒ 新 IP 的计数键不会
//     写进 miniredis ⇒ Exists 断言红。
func TestRateLimiter_PeriodLimit_UnknownFallsBackAndAutoRecovers(t *testing.T) {
	mr, c := newPeriodTestRedis(t)
	rl := newPeriodTestLimiter(t, 5, 1, 0, c)
	ctx := context.Background()
	const ipX = "203.0.113.20"

	// 健康期：qps=5 全放行（第 5 次 HitQuota），第 6 次 OverQuota。
	// Redis 路径不看 burst，Burst=1 不影响本段判据。
	for i := 1; i <= 5; i++ {
		assert.True(t, rl.allow(ctx, scopeGlobal, ipX), "健康期第 %d 次应放行", i)
	}
	assert.False(t, rl.allow(ctx, scopeGlobal, ipX), "健康期超额应拒")
	require.True(t, mr.Exists(redisCountKeyPrefix(scopeGlobal)+":"+ipX), "健康期应走 Redis 路径")

	// 故障期（mr.Close() ⇒ Take 报 (Unknown, err)）：新 IP 回退本地桶，
	// 第 1 次放行（burst 内）、第 2 次拒（本地桶 burst=1 已耗尽 ⇒ 不是 fail-open）
	mr.Close()
	const ipY = "203.0.113.21"
	assert.True(t, rl.allow(ctx, scopeGlobal, ipY), "故障期应回退本地桶放行")
	assert.False(t, rl.allow(ctx, scopeGlobal, ipY), "故障期本地桶 burst 用尽应拒（不是 fail-open）")

	// 恢复期（mr.Restart() ⇒ 同端口同客户端）：缓存实例保留 ⇒ 自动切回 Redis，
	// 新 IP 的计数写进 miniredis；故障期 ipY 的请求从未落过 Redis 计数
	//（Unknown ⇒ 配额不消耗）
	require.NoError(t, mr.Restart())
	const ipZ = "203.0.113.22"
	for i := 1; i <= 5; i++ {
		assert.True(t, rl.allow(ctx, scopeGlobal, ipZ), "恢复后第 %d 次应放行", i)
	}
	assert.False(t, rl.allow(ctx, scopeGlobal, ipZ), "恢复后超额应拒")
	require.True(t, mr.Exists(redisCountKeyPrefix(scopeGlobal)+":"+ipZ),
		"恢复后应自动切回 Redis 路径（新 IP 的计数键应写进 miniredis）")
	assert.False(t, mr.Exists(redisCountKeyPrefix(scopeGlobal)+":"+ipY),
		"故障期的请求不应消耗 Redis 配额（Unknown ⇒ 不落计数）")
}

// TestRateLimiter_PeriodLimit_QuotaHotUpdate 配额热更：同一 Redis 计数窗口内
// 抬高 qps ⇒ 下一次 Take 即按新配额判定（PeriodLimit 快照失配 ⇒ 惰性重建）。
// 与旧实现「每次请求读最新 cfg 比对 n<=qps」同语义：窗口计数连续、阈值即切。
// SetTime 冻结虚拟时钟 ⇒ 整个用例落在同一个窗口内，判据不受真实抖动影响。
//
// 负对照：撤快照失配重建（quota 缓存不更新）⇒ 抬配额后仍按旧配额拒 ⇒
// 更新后的放行断言红。
func TestRateLimiter_PeriodLimit_QuotaHotUpdate(t *testing.T) {
	mr, c := newPeriodTestRedis(t)
	rl := newPeriodTestLimiter(t, 2, 2, 0, c)
	ctx := context.Background()
	const ip = "203.0.113.30"
	key := redisCountKeyPrefix(scopeGlobal) + ":" + ip
	mr.SetTime(time.Now()) // 冻结虚拟时钟：整个用例落在同一窗口内

	// 旧配额 qps=2：2 次放行、第 3 次拒（计数仍在涨：INCRBY 先计数后判定）
	assert.True(t, rl.allow(ctx, scopeGlobal, ip))
	assert.True(t, rl.allow(ctx, scopeGlobal, ip))
	assert.False(t, rl.allow(ctx, scopeGlobal, ip))
	require.Equal(t, "3", mustGet(t, mr, key))

	// 热更 qps=5（同一窗口、计数连续）：第 4、5 次放行（第 5 次 HitQuota）、第 6 次拒
	rl.UpdateConfig(RateLimitConfig{GlobalQPS: 5, Burst: 5, Enabled: true, UseRedis: true})
	assert.True(t, rl.allow(ctx, scopeGlobal, ip), "抬配额后第 4 次应放行")
	assert.True(t, rl.allow(ctx, scopeGlobal, ip), "抬配额后第 5 次（恰满）应放行")
	assert.False(t, rl.allow(ctx, scopeGlobal, ip), "抬配额后第 6 次仍拒")
	assert.Equal(t, "6", mustGet(t, mr, key))
}

// TestRateLimiter_FallbackObserved 阶段 2 收编的新观测面：Redis 故障降级不再
// 静默——每次非 Redis 判定经 WithFallbackMetrics 计数进
// ratelimit_fallback_total{scope,source}（core#83 阶段 2；redisx #118 的动机：
// 「现在到底在 Redis 限还是在本地限」必须可观测）。
//
// 负对照：撤 resilientFor 的 WithFallbackMetrics 接线 ⇒ 计数断言红（降级
// 行为仍在但观测面消失——正是该用例钉住的东西）。
func TestRateLimiter_FallbackObserved(t *testing.T) {
	mr, c := newPeriodTestRedis(t)
	rl := newPeriodTestLimiter(t, 5, 1, 0, c)

	fallbackVec := metrics.RatelimitFallbackVec()
	before := testutil.ToFloat64(fallbackVec.WithLabelValues(string(scopeGlobal), redisxratelimit.SourceLocalFallback.String()))

	// 故障期一次降级判定（本地桶路径，同 UnknownFallsBack 用例的注入法）
	mr.Close()
	_ = rl.allow(context.Background(), scopeGlobal, "203.0.113.30")

	after := testutil.ToFloat64(fallbackVec.WithLabelValues(string(scopeGlobal), redisxratelimit.SourceLocalFallback.String()))
	assert.Greater(t, after, before, "降级判定必须计数进 ratelimit_fallback_total（撤 WithFallbackMetrics 即红）")
}

// TestRateLimiter_FallbackUsesLocalBucketState 钉住降级口径的**边界**：
// Redis 故障时的放行与否由**本地桶当前余量**决定，不是恒放行。
//
// 机制：本地桶在健康期从未被消耗（Redis 路径不碰它），因此降级瞬间它仍是满的；
// 要证明降级真的读了本地桶状态，必须先把本地桶用光——只能走本地路径
// （SetRedis(nil)），因为 Redis 路径不消耗本地桶。
//
// 判据：本地桶耗尽后制造 Redis 故障 ⇒ 该次判定必须被拒。
//
// 负对照：
//   - 若降级恒放行（不查本地桶，fail-open 误实现）⇒ 拒绝断言红；
//   - 若降级误用 Redis 侧的失败计数 ⇒ 结论相反 ⇒ 红；
//   - 若本地桶工厂返回 nil（上游按 FailMode 恒放行）⇒ 拒绝断言红。
func TestRateLimiter_FallbackUsesLocalBucketState(t *testing.T) {
	mr, c := newPeriodTestRedis(t)
	rl := newPeriodTestLimiter(t, 5, 1, 0, c) // Burst=1：本地桶只有 1 枚令牌
	ctx := context.Background()
	const ip = "203.0.113.50"

	// 先把本地桶的令牌用光：Redis 路径不碰本地桶，只能靠 SetRedis(nil) 走本地路径
	rl.SetRedis(nil)
	assert.True(t, rl.allow(ctx, scopeGlobal, ip), "本地桶首枚令牌应放行")
	assert.False(t, rl.allow(ctx, scopeGlobal, ip), "本地桶耗尽后应拒")

	// 装回 Redis 并制造故障 ⇒ 判定走降级路径 ⇒ 降级看的是当前本地桶余量
	rl.SetRedis(c)
	mr.Close()
	assert.False(t, rl.allow(ctx, scopeGlobal, ip),
		"本地桶已耗尽，降级判定必须拒绝（恒放行 = fail-open 回归）")

	// 降级确实发生过（观测面可证）：本条同时是降级路径的判据，不是「没走降级所以拒」
	fallbackVec := metrics.RatelimitFallbackVec()
	assert.Greater(t,
		testutil.ToFloat64(fallbackVec.WithLabelValues(string(scopeGlobal), redisxratelimit.SourceLocalFallback.String())),
		0.0, "本条应走 local-fallback 降级路径（否则拒绝来自别处，本判据无意义）")
}
