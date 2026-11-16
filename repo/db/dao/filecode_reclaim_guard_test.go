// CountReclaimPathGuard 口径测试（core#172 守卫②降级）：
// 软删行/无账本行必须被数到（挡），active+有 ref 的行不再数（收紧生效）。
package dao

import (
	"context"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/repo/db/model"
)

func newReclaimGuardTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(
		&model.FileCode{}, &model.FileCodeFile{},
		&model.Blob{}, &model.FileReference{},
	))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return g
}

func TestCountReclaimPathGuard_TenShapes(t *testing.T) {
	g := newReclaimGuardTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()
	const p = "uploads/2026/11/16/guard.bin"

	// 覆盖集合的前置：一个 blob 行（ref 挂它下面）
	b := model.Blob{ObjectKey: p, Size: 1, State: model.BlobActive}
	require.NoError(t, g.Create(&b).Error)
	cover := func(fcID uint) {
		require.NoError(t, g.Create(&model.FileReference{
			BlobID: b.ID, FileCodeID: fcID, State: model.RefActive,
		}).Error)
	}

	// ① active 未覆盖主行 ⇒ 数（回填失败面，必须挡）
	fcUncovered := &model.FileCode{Code: "UNC1", FilePath: p, Text: "x"}
	require.NoError(t, g.Create(fcUncovered).Error)
	// ② active 已覆盖主行 ⇒ **不数**（收紧生效；旧口径数 1）
	fcCovered := &model.FileCode{Code: "COV1", FilePath: p, Text: "y"}
	require.NoError(t, g.Create(fcCovered).Error)
	cover(fcCovered.ID)
	// ③ 软删已覆盖主行 ⇒ 数（回收站占位，任何覆盖态）
	fcSoft := &model.FileCode{Code: "SFT1", FilePath: p, Text: "z"}
	require.NoError(t, g.Create(fcSoft).Error)
	cover(fcSoft.ID)
	require.NoError(t, g.Delete(&model.FileCode{}, "code = ?", "SFT1").Error)
	// ④ 软删未覆盖主行 ⇒ 数
	fcSoftUnc := &model.FileCode{Code: "SFT2", FilePath: p, Text: "w"}
	require.NoError(t, g.Create(fcSoftUnc).Error)
	require.NoError(t, g.Delete(&model.FileCode{}, "code = ?", "SFT2").Error)

	n, err := repo.CountReclaimPathGuard(ctx, p)
	require.NoError(t, err)
	// 期望：①+③+④ = 3（② 不数）
	assert.Equal(t, int64(3), n, "软删(2)+未覆盖(1)=3；已覆盖 active 行必须被排除（收紧生效）")

	// 旧口径对照：全量口径数 4（含已覆盖 active 行）——钉住两个口径的差异
	old, err := repo.CountFilePathReferences(ctx, p, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(4), old, "旧口径应数全部 4 行（差异面被钉住）")

	// ⑤ 子表未覆盖 ⇒ 数
	require.NoError(t, g.Create(&model.FileCodeFile{
		FileCodeID: fcUncovered.ID, FileName: "c.bin", UUIDFileName: "c.bin",
		FilePath: p, Size: 1,
	}).Error)
	// ⑥ 子表已覆盖（挂到 covered 分享下）⇒ 不数
	require.NoError(t, g.Create(&model.FileCodeFile{
		FileCodeID: fcCovered.ID, FileName: "d.bin", UUIDFileName: "d.bin",
		FilePath: p, Size: 1,
	}).Error)
	n, err = repo.CountReclaimPathGuard(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, int64(4), n, "子表：未覆盖+1、已覆盖+0")

	// 空路径 ⇒ 0（无对象可挡，与旧口径一致）
	n, err = repo.CountReclaimPathGuard(ctx, "")
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)
}
