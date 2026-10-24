package dao

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// =====================================================================
// 取件计次（2026-10-24）：一个下载令牌至多扣 1 次取件次数
//
// 语义（回归锚点）：
//   - 同一令牌重复使用（多文件逐个下载 / 断点续传 / 重复点击）⇒ 只扣 1 次；
//   - 不同令牌（= 不同取件会话）⇒ 各自扣 1 次；
//   - 次数耗尽 ⇒ 报告 exhausted，且**不**扣次（不把额度白扔）。
// =====================================================================

func newPickupChargeTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}, &model.PickupCharge{}))
	// glebarez/sqlite 的 :memory: 每条连接是独立库 ⇒ 钉死单连接。
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return g
}

func TestChargePickupOnce_SameTokenChargesOnce(t *testing.T) {
	g := newPickupChargeTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()
	require.NoError(t, repo.Create(ctx, &model.FileCode{Code: "C1", ExpiredCount: 3}))

	// 首次：真扣（3→2，used_count 1）
	charged, exhausted, err := repo.ChargePickupOnce(ctx, "C1", "tok-a", "download")
	require.NoError(t, err)
	assert.True(t, charged, "首次使用令牌应扣次")
	assert.False(t, exhausted)

	// 同一令牌再来三次（多文件逐个下载 / 断点续传 / 重复点击）⇒ 都不再扣
	for i := 0; i < 3; i++ {
		charged, exhausted, err = repo.ChargePickupOnce(ctx, "C1", "tok-a", "download")
		require.NoError(t, err)
		assert.False(t, charged, "同一令牌不应重复扣次")
		assert.False(t, exhausted)
	}

	fc, err := repo.GetByCode(ctx, "C1")
	require.NoError(t, err)
	assert.Equal(t, 2, fc.ExpiredCount, "同一令牌只扣 1 次")
	assert.Equal(t, 1, fc.UsedCount)
}

func TestChargePickupOnce_DifferentTokensChargeEach(t *testing.T) {
	g := newPickupChargeTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()
	require.NoError(t, repo.Create(ctx, &model.FileCode{Code: "C2", ExpiredCount: 3}))

	// 三个不同令牌 = 三次独立取件 ⇒ 扣 3 次
	for i, tok := range []string{"tok-1", "tok-2", "tok-3"} {
		charged, exhausted, err := repo.ChargePickupOnce(ctx, "C2", tok, "download")
		require.NoError(t, err)
		assert.True(t, charged, "第 %d 个令牌应各自扣次", i+1)
		assert.False(t, exhausted)
	}

	fc, err := repo.GetByCode(ctx, "C2")
	require.NoError(t, err)
	assert.Equal(t, 0, fc.ExpiredCount, "3 次取件把 3 次额度用完")
	assert.Equal(t, 3, fc.UsedCount)
}

func TestChargePickupOnce_ExhaustedReportedNotCharged(t *testing.T) {
	g := newPickupChargeTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()
	require.NoError(t, repo.Create(ctx, &model.FileCode{Code: "C3", ExpiredCount: 1}))

	charged, exhausted, err := repo.ChargePickupOnce(ctx, "C3", "tok-first", "download")
	require.NoError(t, err)
	assert.True(t, charged)
	assert.False(t, exhausted)

	// 额度已用完：新令牌（= 新的一次取件）⇒ exhausted，且**不**扣次
	charged, exhausted, err = repo.ChargePickupOnce(ctx, "C3", "tok-second", "download")
	require.NoError(t, err)
	assert.False(t, charged)
	assert.True(t, exhausted, "次数用完应报告 exhausted")

	fc, err := repo.GetByCode(ctx, "C3")
	require.NoError(t, err)
	assert.Equal(t, 0, fc.ExpiredCount)
	assert.Equal(t, 1, fc.UsedCount, "exhausted 不应再累加 used_count")

	// 关键：exhausted 那次**不能**留下计次登记 —— 否则该令牌后续会被误判成
	// 「已计次」而免费放行（额度=1 时等于白送一次取件）。
	charged, exhausted, err = repo.ChargePickupOnce(ctx, "C3", "tok-second", "download")
	require.NoError(t, err)
	assert.False(t, charged)
	assert.True(t, exhausted, "exhausted 的令牌重试仍应是 exhausted（登记已回滚）")
}

func TestChargePickupOnce_UnlimitedNeverExhausted(t *testing.T) {
	g := newPickupChargeTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()
	require.NoError(t, repo.Create(ctx, &model.FileCode{Code: "C4", ExpiredCount: -1}))

	for i := 0; i < 5; i++ {
		charged, exhausted, err := repo.ChargePickupOnce(ctx, "C4", "tok-"+string(rune('a'+i)), "download")
		require.NoError(t, err)
		assert.True(t, charged)
		assert.False(t, exhausted, "不限次永不耗尽")
	}

	fc, err := repo.GetByCode(ctx, "C4")
	require.NoError(t, err)
	assert.Equal(t, -1, fc.ExpiredCount, "不限次不变")
	assert.Equal(t, 5, fc.UsedCount)
}

func TestChargePickupOnce_EmptyFingerprintChargesEveryTime(t *testing.T) {
	g := newPickupChargeTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()
	require.NoError(t, repo.Create(ctx, &model.FileCode{Code: "C5", ExpiredCount: 3}))

	// 无令牌指纹（令牌未启用）⇒ 无「取件会话」概念 ⇒ 每次交付都算一次取件
	for i := 0; i < 2; i++ {
		charged, _, err := repo.ChargePickupOnce(ctx, "C5", "", "download")
		require.NoError(t, err)
		assert.True(t, charged)
	}
	fc, err := repo.GetByCode(ctx, "C5")
	require.NoError(t, err)
	assert.Equal(t, 1, fc.ExpiredCount, "无会话键时按每次交付扣次")
}

func TestDeleteExpiredPickupCharges(t *testing.T) {
	g := newPickupChargeTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()
	require.NoError(t, repo.Create(ctx, &model.FileCode{Code: "C6", ExpiredCount: -1}))

	_, _, err := repo.ChargePickupOnce(ctx, "C6", "tok-old", "download")
	require.NoError(t, err)

	// cutoff 在过去 ⇒ 记录比 cutoff 新 ⇒ 不删
	n, err := repo.DeleteExpiredPickupCharges(ctx, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)

	// cutoff 在未来 ⇒ 记录比 cutoff 旧 ⇒ 被清掉
	n, err = repo.DeleteExpiredPickupCharges(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
}
