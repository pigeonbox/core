package dao

import (
	"context"
	"testing"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 存量回填负对照（core#172 第二片）：
//   - 中断再跑不重复计数（幂等、差集驱动）；
//   - 配额口径不因回填漂移（只写账本两表，不碰 file_codes 聚合）。

// TestBackfillLegacyRefs_IdempotentNoDoubleCount 正例 + 幂等：
// 老数据（有路径、无账本行）首次回填补齐；再跑差集为空，不产生重复行。
func TestBackfillLegacyRefs_IdempotentNoDoubleCount(t *testing.T) {
	g := newHardDeleteLedgerTestDB(t)
	repo := NewBlobRepository(g)
	ctx := context.Background()

	// 老数据形状：主表 + 子表都有路径，账本两表为空
	require.NoError(t, g.Create(&model.FileCode{
		Code: "BF00001", FilePath: "uploads/bf/a.bin", Text: "a", Size: 10, ExpiredCount: -1,
	}).Error)
	var fc model.FileCode
	require.NoError(t, g.Where("code = ?", "BF00001").First(&fc).Error)
	require.NoError(t, g.Create(&model.FileCodeFile{
		FileCodeID: fc.ID, FileName: "b.bin", UUIDFileName: "b.bin",
		FilePath: "uploads/bf/b.bin", Size: 20, SortOrder: 1,
	}).Error)

	st1, err := repo.BackfillLegacyRefs(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, st1.Scanned)
	assert.Equal(t, 2, st1.Missing, "主表 + 子表两条路径都缺账本")
	assert.Equal(t, 2, st1.Created)
	assert.Equal(t, 0, st1.Failed)

	var blobs, refs int64
	require.NoError(t, g.Model(&model.Blob{}).Count(&blobs).Error)
	require.NoError(t, g.Model(&model.FileReference{}).Count(&refs).Error)
	assert.Equal(t, int64(2), blobs)
	assert.Equal(t, int64(2), refs)

	// 重入：差集为空，不重复计数（负对照核心）
	st2, err := repo.BackfillLegacyRefs(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, st2.Missing, "第二次跑差集必须为空")
	assert.Equal(t, 0, st2.Created, "回填中断/重跑不得重复计数")

	var blobs2, refs2 int64
	require.NoError(t, g.Model(&model.Blob{}).Count(&blobs2).Error)
	require.NoError(t, g.Model(&model.FileReference{}).Count(&refs2).Error)
	assert.Equal(t, blobs, blobs2, "blob 行数不得增长")
	assert.Equal(t, refs, refs2, "ref 行数不得增长")
}

// TestBackfillLegacyRefs_ResumeAfterInterrupt 模拟中断恢复：
// 只回填了一半（主表路径有账本、子表没有）⇒ 重跑只补缺失的那条。
func TestBackfillLegacyRefs_ResumeAfterInterrupt(t *testing.T) {
	g := newHardDeleteLedgerTestDB(t)
	repo := NewBlobRepository(g)
	ctx := context.Background()

	require.NoError(t, g.Create(&model.FileCode{
		Code: "BF00002", FilePath: "uploads/bf/c.bin", Text: "c", Size: 10, ExpiredCount: -1,
	}).Error)
	var fc model.FileCode
	require.NoError(t, g.Where("code = ?", "BF00002").First(&fc).Error)
	require.NoError(t, g.Create(&model.FileCodeFile{
		FileCodeID: fc.ID, FileName: "d.bin", UUIDFileName: "d.bin",
		FilePath: "uploads/bf/d.bin", Size: 20, SortOrder: 1,
	}).Error)

	// 中断现场：只有主表路径被补上
	_, err := repo.EnsureBlobWithRef(ctx, "uploads/bf/c.bin", 10, "", fc.ID)
	require.NoError(t, err)

	st, err := repo.BackfillLegacyRefs(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, st.Missing, "只缺子表那条")
	assert.Equal(t, 1, st.Created)

	var refs int64
	require.NoError(t, g.Model(&model.FileReference{}).Count(&refs).Error)
	assert.Equal(t, int64(2), refs, "主表已有 1 + 补齐 1，不重复")
}

// TestBackfillLegacyRefs_QuotaDriftFree 配额负对照：
// 回填前后 file_codes 的两个配额口径（逻辑/物理含回收站）逐字节不变。
func TestBackfillLegacyRefs_QuotaDriftFree(t *testing.T) {
	g := newHardDeleteLedgerTestDB(t)
	fcRepo := NewFileCodeRepository(g)
	repo := NewBlobRepository(g)
	ctx := context.Background()

	// 存量：一条存活 + 一条软删（回收站仍占盘的物理口径要覆盖）
	require.NoError(t, g.Create(&model.FileCode{
		Code: "BF00003", FilePath: "uploads/bf/e.bin", Text: "e", Size: 100, ExpiredCount: -1,
	}).Error)
	require.NoError(t, g.Create(&model.FileCode{
		Code: "BF00004", FilePath: "uploads/bf/f.bin", Text: "f", Size: 250, ExpiredCount: -1,
	}).Error)
	var alive, soft model.FileCode
	require.NoError(t, g.Where("code = ?", "BF00003").First(&alive).Error)
	require.NoError(t, g.Where("code = ?", "BF00004").First(&soft).Error)
	require.NoError(t, fcRepo.DeleteByIDTx(ctx, soft.ID)) // 软删进回收站

	logical0, err := fcRepo.GetTotalSize(ctx)
	require.NoError(t, err)
	physical0, err := fcRepo.GetPhysicalObjectUsage(ctx)
	require.NoError(t, err)

	st, err := repo.BackfillLegacyRefs(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, st.Created)
	assert.Equal(t, 0, st.Failed)

	logical1, err := fcRepo.GetTotalSize(ctx)
	require.NoError(t, err)
	physical1, err := fcRepo.GetPhysicalObjectUsage(ctx)
	require.NoError(t, err)
	assert.Equal(t, logical0, logical1, "回填不得漂移逻辑配额口径")
	assert.Equal(t, physical0, physical1, "回填不得漂移物理配额口径")
	assert.Equal(t, int64(100), logical1, "软删行不计逻辑口径")
	assert.Equal(t, int64(350), physical1, "物理口径含回收站")
}

// TestBackfillLegacyRefs_SoftDeletedShareGetsRef 软删行也要回填：
// 回收站行仍占对象，硬删时必须有 active ref 可释放。
func TestBackfillLegacyRefs_SoftDeletedShareGetsRef(t *testing.T) {
	g := newHardDeleteLedgerTestDB(t)
	fcRepo := NewFileCodeRepository(g)
	repo := NewBlobRepository(g)
	ctx := context.Background()

	require.NoError(t, g.Create(&model.FileCode{
		Code: "BF00005", FilePath: "uploads/bf/g.bin", Text: "g", Size: 10, ExpiredCount: -1,
	}).Error)
	var fc model.FileCode
	require.NoError(t, g.Where("code = ?", "BF00005").First(&fc).Error)
	require.NoError(t, fcRepo.DeleteByIDTx(ctx, fc.ID)) // 软删

	st, err := repo.BackfillLegacyRefs(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, st.Created, "软删行必须回填（Unscoped 口径）")

	var ref model.FileReference
	require.NoError(t, g.Where("file_code_id = ?", fc.ID).First(&ref).Error)
	assert.Equal(t, model.RefActive, ref.State, "回填的引用必须是 active（软删不释放）")
}

// TestBackfillLegacyRefs_TextShareSkipped 文本分享无对象，不回填。
func TestBackfillLegacyRefs_TextShareSkipped(t *testing.T) {
	g := newHardDeleteLedgerTestDB(t)
	repo := NewBlobRepository(g)
	ctx := context.Background()

	require.NoError(t, g.Create(&model.FileCode{
		Code: "BF00006", Text: "正文内容", ExpiredCount: -1,
	}).Error)

	st, err := repo.BackfillLegacyRefs(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, st.Scanned, "文本分享不进扫描集")

	var refs int64
	require.NoError(t, g.Model(&model.FileReference{}).Count(&refs).Error)
	assert.Equal(t, int64(0), refs)
}
