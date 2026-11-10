package dao

// InsightStats 软删口径守卫（core#107）。
//
// 修复前两个口径同时失真：total/totalSize 被 GORM 软删 scope 悄悄排除软删行
// （恒等于 alive），softDeleted 的 "deleted_at IS NOT NULL" 又与 scope 自相
// 矛盾（恒 0）。本用例钉住四个数字：
//   - total/totalSize 含软删行（purge 前仍占存储）；
//   - softDeleted 真实计数；
//   - alive 只数存活行；
//   - purge 后四数同步收敛。
//
// 负对照：去掉任一 Unscoped() ⇒ 对应断言当场红。

import (
	"context"
	"testing"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInsightStats_SoftDeleteAccounting(t *testing.T) {
	g := newGovernanceTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	require.NoError(t, repo.Create(ctx, &model.FileCode{Code: "INSGHT01", Size: 10}))
	require.NoError(t, repo.Create(ctx, &model.FileCode{Code: "INSGHT02", Size: 20}))
	soft := &model.FileCode{Code: "INSGHT03", Size: 30}
	require.NoError(t, repo.Create(ctx, soft))
	require.NoError(t, repo.Delete(ctx, soft.ID))

	total, totalSize, alive, softDeleted, err := repo.InsightStats(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 3, total, "total 必须含软删行（未 purge 全量）")
	assert.EqualValues(t, 60, totalSize, "totalSize 必须含软删行（purge 前仍占存储）")
	assert.EqualValues(t, 2, alive, "alive 只数存活行")
	assert.EqualValues(t, 1, softDeleted, "回收站行数必须真实计数（修复前恒 0）")

	// purge 后四数收敛（软删行不再占任何口径）
	_, err = repo.HardDeleteByIDs(ctx, []uint{soft.ID})
	require.NoError(t, err)
	total, totalSize, alive, softDeleted, err = repo.InsightStats(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 2, total)
	assert.EqualValues(t, 30, totalSize)
	assert.EqualValues(t, 2, alive)
	assert.EqualValues(t, 0, softDeleted)
}
