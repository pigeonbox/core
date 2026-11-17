// filecode_pickup_charge_concurrent_test.go 取件计次的并发语义（L5 层）
//
// ## 背景
//
// ChargePickupOnce 的语义是「一个下载令牌（取件会话）至多扣 1 次取件次数」，
// 顺序上**先登记（唯一索引裁决「首次」）再原子扣减**：
//
//	CreatePickupCharge   INSERT ... ON CONFLICT DO NOTHING  ← 唯一索引裁决首次
//	DecrementExpiredCount UPDATE ... WHERE expired_count>0 ← 原子扣减
//	（扣不动 ⇒ 撤销登记 ⇒ 报告 exhausted；扣次报错 ⇒ 撤销登记 ⇒ 上抛）
//
// 既有测试 filecode_pickup_charge_test.go **全是顺序用例**：单线程跑完一次再
// 跑下一次 ⇒ 「恰好 1 个扣成功」是串行执行的必然结果，**证明不了任何并发性质**。
// 本文件把同一批语义放到**真并发**下验。
//
// ## 为什么用临时文件 SQLite（而不是既有测试的 :memory: + SetMaxOpenConns(1)）
//
// glebarez/sqlite 的 `:memory:` **每条连接是独立库** ⇒ 必须钉死单连接 ⇒
// 所有 DB 访问被完全串行化 ⇒ 并发用例碰不到「唯一索引裁决」与「原子 UPDATE
// 争用」这两条真实语义（那样跑出来的「恰好 1 个成功」是串行的必然结果）。
//
// 这里改用 t.TempDir() 里的**临时文件**库：
//   - 多条连接共享同一个库文件 ⇒ 真并发；
//   - WAL + busy_timeout(10s) ⇒ 写锁争用会等待，而不是立刻报 SQLITE_BUSY；
//   - SetMaxOpenConns(8) ⇒ 允许 8 条连接同时在飞（不是 1 ⇒ 也不是完全串行）。
//
// ## 负对照（必须能变红）
//
// 抽掉唯一索引裁决（把 CreatePickupCharge 改成**先查后插**的 check-then-act：
// 先 SELECT 查有没有，再不带 OnConflict 地 INSERT）⇒
//   - TestChargePickupOnce_ConcurrentSameTokenChargesExactlyOnce 红：
//     多个 goroutine 同时「查不到」⇒ 一起去 INSERT ⇒ 撞唯一索引报错
//     （RowsAffected 判定失效）⇒ 断言「err 数=0」与「恰好 1 个 charged」双双破。
//   - 再把 model.PickupCharge 的 uniqueIndex 一起去掉 ⇒ 连唯一索引这道防线也没了
//     ⇒ 多个 goroutine 都登记成功 ⇒ **多个都扣次** ⇒ 「恰好 1 个 charged」直接破
//     （超扣：一次取件扣掉多次配额）。
//
// 断言口径见各用例注释。
package dao

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// newPickupChargeConcurrentTestDB 构造一个**真并发**的 SQLite 测试库（见文件头
// 「为什么用临时文件 SQLite」）。
func newPickupChargeConcurrentTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "pickup_charge.db") +
		"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)"
	g, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}, &model.PickupCharge{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(8)
	sqlDB.SetMaxIdleConns(8)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return g
}

// pickupChargeOutcome 一次 ChargePickupOnce 的结果（并发收集用）。
type pickupChargeOutcome struct {
	charged   bool
	exhausted bool
	err       error
}

// runConcurrentCharges n 个 goroutine 同时对同一分享调 ChargePickupOnce。
//
// sameToken=true  ⇒ 全部用**同一个** tokenHash（同一取件会话，语义上应只扣 1 次）；
// sameToken=false ⇒ 第 i 个用 tok-%03d（互不相同的取件会话，语义上各扣 1 次）。
func runConcurrentCharges(t *testing.T, repo *FileCodeRepository, code string, n int, sameToken bool) []pickupChargeOutcome {
	t.Helper()
	ctx := context.Background()
	out := make([]pickupChargeOutcome, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 同步起跑：避免 goroutine 逐个启动把并发退化成串行
			tok := fmt.Sprintf("tok-%03d", i)
			if sameToken {
				tok = "tok-same"
			}
			charged, exhausted, err := repo.ChargePickupOnce(ctx, code, tok, "download", "10.0.0.1")
			out[i] = pickupChargeOutcome{charged: charged, exhausted: exhausted, err: err}
		}(i)
	}
	close(start)
	wg.Wait()
	return out
}

// summarizeOutcomes 把并发结果归成「成功/耗尽/出错」三档计数。
func summarizeOutcomes(out []pickupChargeOutcome) (charged, exhausted, failed int, errs []error) {
	for i, o := range out {
		if o.err != nil {
			failed++
			errs = append(errs, fmt.Errorf("#%d: %w", i, o.err))
		}
		if o.charged {
			charged++
		}
		if o.exhausted {
			exhausted++
		}
	}
	return charged, exhausted, failed, errs
}

// TestChargePickupOnce_ConcurrentSameTokenChargesExactlyOnce 同一令牌并发扣次。
//
// 语义：「一个下载令牌至多扣 1 次取件次数」。
//
// 缺陷形态（若裁决失效）：N 个 goroutine 同时用同一令牌到达 ⇒ 登记阶段
// check-then-act ⇒ 都判定自己「是首次」⇒ 各自去扣一次 ⇒ **超扣**（一次取件
// 扣掉 N 次配额）。
//
// 断言口径：
//   - 恰好 1 个 charged=true（其余 charged=false）；
//   - 剩余次数只被扣 1 次、used_count 只累加 1；
//   - pickup_charges 表里该分享**恰 1 行**（唯一索引裁决的可见证据）。
func TestChargePickupOnce_ConcurrentSameTokenChargesExactlyOnce(t *testing.T) {
	g := newPickupChargeConcurrentTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()
	require.NoError(t, repo.Create(ctx, &model.FileCode{Code: "CC-SAME", ExpiredCount: 10}))

	const n = 50
	out := runConcurrentCharges(t, repo, "CC-SAME", n, true)

	charged, exhausted, failed, errs := summarizeOutcomes(out)
	require.Zero(t, failed, "并发下不得出现错误：%v", errs)
	assert.Equal(t, 1, charged, "同一令牌并发：必须**恰好 1 个**请求真扣次（超扣）")
	assert.Equal(t, 0, exhausted, "额度充足时不得报 exhausted")

	fc, err := repo.GetByCode(ctx, "CC-SAME")
	require.NoError(t, err)
	assert.Equal(t, 9, fc.ExpiredCount, "同一令牌只扣 1 次（10 → 9）")
	assert.Equal(t, 1, fc.UsedCount, "used_count 只累加 1")

	var rows int64
	require.NoError(t, g.Model(&model.PickupCharge{}).Where("code = ?", "CC-SAME").Count(&rows).Error)
	assert.Equal(t, int64(1), rows, "pickup_charges 表里该分享恰 1 行（唯一索引裁决）")
}

// TestChargePickupOnce_ConcurrentDistinctTokensExhaustBoundary 异令牌并发 + 次数耗尽边界。
//
// 语义：不同令牌 = 不同取件会话 ⇒ 各自算一次取件；次数耗尽 ⇒ 报 exhausted 且
// **不扣次**（不把额度白扔）。
//
// 缺陷形态（若扣减非原子/边界判定有洞）：剩余次数=1、M 个并发 ⇒ 若扣减不是
// 「UPDATE ... WHERE expired_count>0」这种单语句原子判定，就会出现
// 「两人都扣成功」（超扣）或「扣成功但表里没扣」（漏扣/口径错）。
//
// 断言口径：
//   - 恰好 1 个 charged；
//   - 其余全部 exhausted=true；
//   - 剩余次数归零（不得扣成负数）、used_count 只累加 1；
//   - exhausted 的那些必须**撤销登记** ⇒ 表里只剩 1 行
//     （否则该令牌后续会被误判成「已计次」⇒ 免费放行一次）。
func TestChargePickupOnce_ConcurrentDistinctTokensExhaustBoundary(t *testing.T) {
	g := newPickupChargeConcurrentTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()
	require.NoError(t, repo.Create(ctx, &model.FileCode{Code: "CC-EXH", ExpiredCount: 1}))

	const m = 20
	out := runConcurrentCharges(t, repo, "CC-EXH", m, false)

	charged, exhausted, failed, errs := summarizeOutcomes(out)
	require.Zero(t, failed, "并发下不得出现错误：%v", errs)
	assert.Equal(t, 1, charged, "剩余次数=1：必须**恰好 1 个**扣成功（不得两人都扣成功）")
	assert.Equal(t, m-1, exhausted, "其余必须全部 exhausted=true（不得出现「扣成功但没扣」）")

	fc, err := repo.GetByCode(ctx, "CC-EXH")
	require.NoError(t, err)
	assert.Equal(t, 0, fc.ExpiredCount, "剩余次数归零（不得扣成负数）")
	assert.Equal(t, 1, fc.UsedCount, "只累加 1 次")

	var rows int64
	require.NoError(t, g.Model(&model.PickupCharge{}).Where("code = ?", "CC-EXH").Count(&rows).Error)
	assert.Equal(t, int64(1), rows, "exhausted 的那些必须撤销登记（残留会让该令牌后续被误判为已计次 ⇒ 免费放行）")
}

// TestChargePickupOnce_ConcurrentDecrementFailureDoesNotConsumeCount
// 并发下「失败不扣次数」的不变量不被破坏。
//
// 语义（回归锚点，与 filecode_pickup_charge_test.go 的 RollbackFailureSurfaced
// 同口径）：扣次失败 ⇒ 撤销登记 ⇒ 只回原始错误（fail-closed）⇒ **不消耗配额**；
// 撤销成功后，该令牌后续重试仍算**一次新取件**（不是「已计次」）。
//
// 缺陷形态（若撤销不成功/不撤销）：计次行残留 ⇒ 同一令牌重试被判「已计次」
// ⇒ 该次取件不消耗配额 ⇒ 「最多取 N 次」被突破一次。
//
// 这里用故障注入让**扣次 UPDATE** 失败（撤销路径正常）⇒ 并发下每个请求都走
// 「扣次失败 ⇒ 撤销登记 ⇒ 上抛」分支。
func TestChargePickupOnce_ConcurrentDecrementFailureDoesNotConsumeCount(t *testing.T) {
	g := newPickupChargeConcurrentTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()
	require.NoError(t, repo.Create(ctx, &model.FileCode{Code: "CC-FAIL", ExpiredCount: 5}))

	// 故障注入：扣次（UPDATE）失败 ⇒ 走「撤销登记」分支。
	// 注入/移除复用既有测试的 injectStmtFailure（同一包）。
	injectStmtFailure(t, g, "update")

	const n = 10
	out := runConcurrentCharges(t, repo, "CC-FAIL", n, false)

	// 移除注入：后面的重试要能真扣次（证明「撤销成功后重试仍算一次新取件」）。
	require.NoError(t, g.Callback().Update().Remove("test:fail_update"), "移除故障注入")

	for i, o := range out {
		require.Error(t, o.err, "第 %d 个请求：扣次失败必须上抛（不得静默吞错）", i)
		assert.False(t, o.charged, "第 %d 个请求：失败路径不得报 charged", i)
		assert.False(t, o.exhausted, "第 %d 个请求：扣次失败不是 exhausted（两者口径必须区分）", i)
	}

	fc, err := repo.GetByCode(ctx, "CC-FAIL")
	require.NoError(t, err)
	assert.Equal(t, 5, fc.ExpiredCount, "失败**不消耗**取件次数")
	assert.Equal(t, 0, fc.UsedCount, "失败不累加 used_count")

	var rows int64
	require.NoError(t, g.Model(&model.PickupCharge{}).Where("code = ?", "CC-FAIL").Count(&rows).Error)
	assert.Equal(t, int64(0), rows, "撤销登记必须全部成功（不留残留计次行）")

	// 撤销成功 ⇒ 该令牌后续重试仍算一次**新取件**（不被误判成「已计次」）。
	charged, exhausted, err := repo.ChargePickupOnce(ctx, "CC-FAIL", "tok-000", "download", "10.0.0.1")
	require.NoError(t, err)
	assert.True(t, charged, "撤销成功后重试应真扣次")
	assert.False(t, exhausted)

	fc, err = repo.GetByCode(ctx, "CC-FAIL")
	require.NoError(t, err)
	assert.Equal(t, 4, fc.ExpiredCount, "重试扣 1 次（5 → 4）")
	assert.Equal(t, 1, fc.UsedCount)
}

// TestPickupCharge_UniqueIndexArbitratesSameToken 计次登记的**唯一索引**必须
// 真的存在并裁决「首次」——它是「同一令牌只扣 1 次」的唯一防线。
//
// 负对照：把 model.PickupCharge 的 uniqueIndex 去掉 ⇒ 本用例红（重复登记不再
// 被拒）；只把 CreatePickupCharge 改成 check-then-act（索引还在）⇒ 本用例仍绿，
// 但并发用例红（见文件头）。
func TestPickupCharge_UniqueIndexArbitratesSameToken(t *testing.T) {
	g := newPickupChargeConcurrentTestDB(t)
	now := time.Now()
	require.NoError(t, g.Create(&model.PickupCharge{
		Code: "UIDX", TokenHash: "tok-1", Channel: "download", IP: "10.0.0.1", ChargedAt: now,
	}).Error)

	dup := &model.PickupCharge{
		Code: "UIDX", TokenHash: "tok-1", Channel: "download", IP: "10.0.0.1", ChargedAt: now,
	}
	err := g.Create(dup).Error
	require.Error(t, err,
		"同 (code, token_hash) 的重复计次登记必须被唯一索引拒掉（这是「同一令牌只扣 1 次」的唯一防线）")
}
