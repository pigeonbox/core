// lookup_storm_test.go —— 匿名取件码解析的**重复请求风暴**防护（L5 层）
//
// ## 场景
//
// 同一请求被并发重复提交（重放 / 枚举 / 客户端重试）⇒ 取件码解析这条路必须能
// **合并**同码并发，而不是把 N 次重复提交放大成 N 次 DB 回源。
//
// app/anonymous 的 lookupShareCode 在「KV 未命中 ⇒ DB 回源」这条路上用了
// singleflight 合并同码并发（s.flights.Do("anon:lookup:"+code, ...)）——这是
// **限流面**：取件码枚举/重放风暴下，同一个码的 N 次并发只应回源一次。
//
// ## 既有覆盖的缺口（逐条核对过）
//
// app/anonymous 既有用例（anonymous_test.go / memory_mode_test.go /
// lookup_fold_test.go / mint_collision_test.go / redis_degrade_test.go /
// pickup_limit_test.go）**没有一条**断言这条合并语义：
//   - GenerateCode_Concurrent 只验「并发铸造的码互不冲突」（另一条链）；
//   - LookupShareCode_NegativeCache / DBBackfill 只验**顺序**两三次查；
//   - RedisDegrade_* 只验故障降级。
//
// ⇒ 把 singleflight 摘掉（退回每次各自回源），现有用例**一条都不会红** ⇒
// 「风暴不放大 DB」这条保证目前是**裸奔**的。
//
// ## 本文件钉的不变量
//
// 并发 N 次查**同一个**未知码 ⇒ DB 回源次数 == **单次回源的基线**（不放大 N 倍）。
//
// 断言写成「等于基线」而不是写死数字：回源内部查几次（取件码列 → 分享码列 →
// 折叠兜底）取决于配置（conf.CodeFoldEnabledOrDefault）与码长 ⇒ 基线由本文件
// 先单跑一次量出来 ⇒ 断言只关心「有没有被放大」。
//
// ## 负对照（必须能变红，实跑过）
//
// 把 lookupShareCode 里的 s.flights.Do(...) 换成直接调用 resolveShareCodeFromDB
// （摘掉 singleflight 合并）⇒ 并发 N 次 ⇒ DB 回源次数 == N×基线 ⇒ 本用例红。
//
// ## 为什么用真并发 DB + 栅栏
//
// ① 既有 app/anonymous 测试用 :memory: + SetMaxOpenConns(1) ⇒ DB 访问被串行化
//
//	⇒ N 次回源无法同时在飞 ⇒ 撤掉 singleflight 后也会被连接池排队掩盖 ⇒ 负对照
//	变不成红。本文件自建**临时文件** SQLite（多连接 + WAL + busy_timeout）。
//
// ② 栅栏（barrier）：把每个 DB 查询挡在「执行 SQL 之前」，直到 N 个请求都到达
//
//	才有第一个真正执行 ⇒ 保证「N 个请求全部越过了负缓存检查」⇒ 撤掉
//	singleflight 时 N 次回源**真的**都发生（否则先完成的请求会写负缓存，
//	后到的请求直接吃缓存 ⇒ 负对照时有时无 ⇒ 假绿）。
package anonymous

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// dbProbe 观测「DB 回源」次数的探针 + 栅栏。
//
// 装在 gorm 的 Query 回调链上（Before "gorm:query" ⇒ 在 SQL 执行前、在拿到
// 连接前触发 ⇒ 被挡住时**不占用连接** ⇒ 多个请求能同时到达栅栏）。
type dbProbe struct {
	mu       sync.Mutex
	count    int // 累计查询数
	arrivals int // 累计到达数
	target   int // 栅栏目标（= 并发请求数）
	release  chan struct{}
	once     sync.Once
	armed    bool // 是否已武装栅栏
}

// onQuery gorm Query 回调：计数 + 栅栏。
func (p *dbProbe) onQuery(tx *gorm.DB) {
	p.mu.Lock()
	p.count++
	p.arrivals++
	arrivals := p.arrivals
	armed := p.armed
	p.mu.Unlock()
	if !armed {
		return
	}
	if arrivals >= p.target {
		p.once.Do(func() { close(p.release) })
		return
	}
	// 未达目标 ⇒ 挡住本次查询（等释放或超时）。
	// 超时兜底：目标本就到不了时（如 singleflight 合并生效 ⇒ 只有 1 次回源）
	// 不能把测试挂死 ⇒ 超时后放行。
	select {
	case <-p.release:
	case <-time.After(120 * time.Millisecond):
	}
}

// reset 清零计数与到达数（阶段之间切换用）。
func (p *dbProbe) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.count = 0
	p.arrivals = 0
}

// arm 武装栅栏：目标 target 个请求到达后才放行。
func (p *dbProbe) arm(target int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.target = target
	p.armed = true
	p.once = sync.Once{}
	p.release = make(chan struct{})
}

// disarm 解除栅栏（后续查询不再被挡）。
func (p *dbProbe) disarm() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.armed = false
}

// newLookupStormService 构造**真并发**的 anonymous service（临时文件 SQLite +
// 多连接 + miniredis 作 KV）。
func newLookupStormService(t *testing.T) (*Service, *miniredis.Miniredis, *gorm.DB) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	dsn := filepath.Join(t.TempDir(), "lookup-storm.db") +
		"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)"
	g, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}, &model.FileCodeFile{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(8)
	sqlDB.SetMaxIdleConns(8)
	t.Cleanup(func() { _ = sqlDB.Close() })

	return NewService(rdb, g), mr, g
}

// TestLookupShareCode_ConcurrentSameCodeCollapsesToSingleDBSource
// 并发查同一个码 ⇒ DB 回源**不被放大**（singleflight 合并生效）。
//
// 缺陷形态（合并被摘掉）：N 个并发重复提交 ⇒ N 次 DB 回源 ⇒
//   - 取件码枚举/重放风暴把 DB 打穿（放大 N 倍的查询）；
//   - 与负缓存互相打架：先完成的请求写下负缓存，后到的请求吃到缓存 ⇒
//     「有时放大、有时不放大」⇒ 风暴防护时有时无。
func TestLookupShareCode_ConcurrentSameCodeCollapsesToSingleDBSource(t *testing.T) {
	svc, _, g := newLookupStormService(t)
	ctx := context.Background()

	probe := &dbProbe{}
	require.NoError(t, g.Callback().Query().Before("gorm:query").Register("test:lookup_storm_probe", probe.onQuery))

	// 基线：单次回源查几次（取决于码长与折叠配置 ⇒ 不写死数字）。
	const baselineCode = "BASE001"
	_, err := svc.lookupShareCode(ctx, baselineCode)
	require.ErrorIs(t, err, ErrCodeNotFound, "未知码必须回源后报不存在")

	probe.mu.Lock()
	baseline := probe.count
	probe.mu.Unlock()
	require.Greater(t, baseline, 0, "基线必须真的打到了 DB（否则本用例没有判据）")

	// 并发阶段：N 个请求查**另一个**未知码（避开基线码留下的负缓存）。
	const n = 20
	const stormCode = "STORM01"
	probe.reset()
	probe.arm(n)
	defer probe.disarm()

	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 同步起跑：避免逐个启动把并发退化成串行
			_, err := svc.lookupShareCode(ctx, stormCode)
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		require.ErrorIs(t, err, ErrCodeNotFound,
			"第 %d 个请求：未知码必须报不存在（不得因合并而拿到别的结果）", i)
	}

	probe.mu.Lock()
	got := probe.count
	probe.mu.Unlock()

	assert.Equal(t, baseline, got,
		"并发 %d 次查同一个码：DB 回源次数必须等于单次回源的基线 %d（放大即风暴防护失效）",
		n, baseline)
}
