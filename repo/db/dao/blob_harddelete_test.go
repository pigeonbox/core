package dao

import (
	"context"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 硬删全链接入（core#172 第二片）：HardDeleteByIDs / HardDeleteByCode 的
// 行删除与账本引用释放**同事务**——归零转 trashed，未归零保持 active。

// newHardDeleteLedgerTestDB 硬删账本测试库：四表齐全（含 file_code_files
// 子表——hardDeleteChildRows 要删它）。
func newHardDeleteLedgerTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(
		&model.Blob{}, &model.FileReference{},
		&model.FileCode{}, &model.FileCodeFile{},
	))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return g
}

// seedLedgerShare 造「分享 + blob(active) + ref(active)」三件套。
func seedLedgerShare(t *testing.T, g *gorm.DB, code, path string) (shareID uint, blobID uint) {
	t.Helper()
	fc := &model.FileCode{Code: code, FilePath: path, Text: code, Size: 1, ExpiredCount: -1}
	require.NoError(t, g.Create(fc).Error)
	b := model.Blob{ObjectKey: path, Size: 1, State: model.BlobActive}
	require.NoError(t, g.Create(&b).Error)
	require.NoError(t, g.Create(&model.FileReference{
		BlobID: b.ID, FileCodeID: fc.ID, State: model.RefActive,
	}).Error)
	return fc.ID, b.ID
}

// 正例：硬删（回收站 → 彻底删除）同事务释放引用，归零 ⇒ blob 转 trashed。
func TestHardDeleteByIDs_ReleasesLedgerRefs(t *testing.T) {
	g := newHardDeleteLedgerTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	shareID, blobID := seedLedgerShare(t, g, "HDL00001", "uploads/hdl/a.bin")
	// 前置：先软删（硬删只认回收站行）
	require.NoError(t, repo.DeleteByIDTx(ctx, shareID))

	n, err := repo.HardDeleteByIDs(ctx, []uint{shareID})
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	var ref model.FileReference
	require.NoError(t, g.Where("file_code_id = ?", shareID).First(&ref).Error)
	assert.Equal(t, model.RefReleased, ref.State, "硬删必须释放引用")
	require.NotNil(t, ref.ReleasedAt)

	b := reloadBlob(t, g, blobID)
	assert.Equal(t, model.BlobTrashed, b.State, "引用归零 ⇒ blob 进 trashed（宽限期起点）")
	require.NotNil(t, b.TrashedAt)
}

// 负对照①：软删绝不释放引用（回收站行仍占对象，恢复后对象必须在）。
func TestSoftDelete_KeepsLedgerRefsActive(t *testing.T) {
	g := newHardDeleteLedgerTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	shareID, blobID := seedLedgerShare(t, g, "HDL00002", "uploads/hdl/b.bin")
	require.NoError(t, repo.DeleteByIDTx(ctx, shareID)) // 软删

	var ref model.FileReference
	require.NoError(t, g.Where("file_code_id = ?", shareID).First(&ref).Error)
	assert.Equal(t, model.RefActive, ref.State, "软删不得释放引用")
	b := reloadBlob(t, g, blobID)
	assert.Equal(t, model.BlobActive, b.State, "软删后 blob 必须保持 active")
}

// 负对照②：共享对象硬删其一 ⇒ 另一分享的引用仍存活，blob 不进回收。
func TestHardDeleteByIDs_SharedObjectStaysActive(t *testing.T) {
	g := newHardDeleteLedgerTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	const shared = "uploads/hdl/shared.bin"
	shareA, blobID := seedLedgerShare(t, g, "HDL00003", shared)
	// 同对象第二分享（复用同一 blob 行——EnsureBlobWithRef 语义）
	fcB := &model.FileCode{Code: "HDL00004", FilePath: shared, Text: "b", Size: 1, ExpiredCount: -1}
	require.NoError(t, g.Create(fcB).Error)
	require.NoError(t, g.Create(&model.FileReference{
		BlobID: blobID, FileCodeID: fcB.ID, State: model.RefActive,
	}).Error)

	require.NoError(t, repo.DeleteByIDTx(ctx, shareA))
	n, err := repo.HardDeleteByIDs(ctx, []uint{shareA})
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	b := reloadBlob(t, g, blobID)
	assert.Equal(t, model.BlobActive, b.State,
		"引用未归零（另一分享仍引用）⇒ blob 不得转 trashed")

	// A 的引用已释放、B 的仍 active
	var refA, refB model.FileReference
	require.NoError(t, g.Where("file_code_id = ?", shareA).First(&refA).Error)
	require.NoError(t, g.Where("file_code_id = ?", fcB.ID).First(&refB).Error)
	assert.Equal(t, model.RefReleased, refA.State)
	assert.Equal(t, model.RefActive, refB.State)
}

// 负对照③：账本表未迁移（老库/测试库）⇒ 硬删照常成功，不因账本面阻断主链。
func TestHardDeleteByIDs_NoLedgerTable_StillDeletes(t *testing.T) {
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}, &model.FileCodeFile{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	fc := &model.FileCode{Code: "HDL00005", FilePath: "uploads/hdl/c.bin", Text: "c", Size: 1, ExpiredCount: -1}
	require.NoError(t, g.Create(fc).Error)
	require.NoError(t, repo.DeleteByIDTx(ctx, fc.ID))

	n, err := repo.HardDeleteByIDs(ctx, []uint{fc.ID})
	require.NoError(t, err, "账本缺表不得阻断硬删")
	assert.Equal(t, int64(1), n)
}
