package dao

import (
	"context"
	"testing"
	"time"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 明细留存期**不能**跟着令牌 TTL 走（2026-10-24 缺陷）。
//
// 取件记录表同时是「同一令牌不重复计次」的去重键与 owner 侧「取件历史」的
// 逐条明细 ⇒ 若清理窗口取「令牌 TTL + 1h」，owner 看到的明细**最多存在 1 小时**
// ⇒ 第二天打开取件历史明细全空 ⇒ 「逐条取件历史」形同虚设。
func TestDeleteExpiredPickupCharges_RetentionNotTokenTTL(t *testing.T) {
	g := newPickupChargeTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()
	require.NoError(t, repo.Create(ctx, &model.FileCode{Code: "KEEP", ExpiredCount: -1}))

	// 一条「昨天」的明细（模拟取件后过了 1 天）
	_, _, err := repo.ChargePickupOnce(ctx, "KEEP", "tok-old", "download", "1.1.1.1")
	require.NoError(t, err)
	require.NoError(t, g.Exec("update pickup_charges set charged_at = ?", time.Now().Add(-24*time.Hour)).Error)

	// 令牌 TTL 级的窗口（1 小时前）⇒ 明细会被删掉 ⇒ **这就是缺陷本身**
	n, err := repo.DeleteExpiredPickupCharges(ctx, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "令牌 TTL 级的窗口会删掉昨天的明细（缺陷的成因）")

	// 重新造一条，改用 30 天的留存窗口 ⇒ 明细保留 ✔
	_, _, err = repo.ChargePickupOnce(ctx, "KEEP", "tok-old2", "download", "1.1.1.1")
	require.NoError(t, err)
	require.NoError(t, g.Exec("update pickup_charges set charged_at = ?", time.Now().Add(-24*time.Hour)).Error)
	n, err = repo.DeleteExpiredPickupCharges(ctx, time.Now().AddDate(0, 0, -30))
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "30 天留存窗口下昨天的明细必须保留")

	// 0 = 永久保留
	n, err = repo.DeleteExpiredPickupCharges(ctx, time.Time{})
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "0（永久）不应删除任何明细")
}

// 分享被硬删后，其明细是孤儿（没有主人可看）⇒ 应随清理一并删掉；
// 软删（回收站）不算孤儿。
func TestDeleteOrphanPickupCharges(t *testing.T) {
	g := newPickupChargeTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	// 活着的分享
	require.NoError(t, repo.Create(ctx, &model.FileCode{Code: "ALIVE", ExpiredCount: -1}))
	_, _, err := repo.ChargePickupOnce(ctx, "ALIVE", "tok-a", "download", "1.1.1.1")
	require.NoError(t, err)

	// 硬删的分享（先建后删）
	require.NoError(t, repo.Create(ctx, &model.FileCode{Code: "GONE", ExpiredCount: -1}))
	_, _, err = repo.ChargePickupOnce(ctx, "GONE", "tok-b", "download", "1.1.1.1")
	require.NoError(t, err)
	require.NoError(t, g.Exec("delete from file_codes where code = ?", "GONE").Error)

	n, err := repo.DeleteOrphanPickupCharges(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "硬删分享的明细应被清掉")

	var left int64
	require.NoError(t, g.Model(&model.PickupCharge{}).Where("code = ?", "ALIVE").Count(&left).Error)
	assert.Equal(t, int64(1), left, "活着的分享的明细必须保留")
}
