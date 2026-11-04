package dao

import (
	"context"
	"testing"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── 硬删必须带走子表行（core#62）────────────────────────────────────────
//
// file_code_files 是「文件清单的唯一真相源」（v0.32.14 起），与主表**同生共死**。
// 此前**单条**硬删路径 HardDeleteByCode 只删主表 ⇒ 子行永久残留（孤儿行）：
//   ① DB 层面：子行指向已不存在的父行（真机实测已出现存量孤儿行）；
//   ② 存储层面：janitor 的对账引用集（ListAllPathsIncludingDeleted，Unscoped）
//      含这些子行路径 ⇒ 被硬删分享的物理文件仍被当作「有引用」
//      ⇒ 永不回收（远端后端 = 永久泄漏）。
// 批量路径 HardDeleteByIDs 早就删子行 ⇒ 单条路径与之口径不一致。
//
// 顺序要求（core#62 正文）：先查出父行 id → 再删子表 → 最后删父行（同一事务），
// 否则中途失败会留下「父没了子还在」的半删状态。

// seedShareWithChildren 建一个带 n 个子行的分享（父子齐全）。
func seedShareWithChildren(t *testing.T, repo *FileCodeRepository, ctx context.Context, code string, uid uint, n int) *model.FileCode {
	t.Helper()
	fc := &model.FileCode{
		Code:     code,
		UserID:   &uid,
		FilePath: "uploads/" + code + "/a.txt",
	}
	require.NoError(t, repo.Create(ctx, fc))
	childRepo := NewFileCodeFileRepository(repo.db())
	for i := 0; i < n; i++ {
		require.NoError(t, childRepo.CreateBatch(ctx, []*model.FileCodeFile{{
			FileCodeID:   fc.ID,
			FileName:     "a.txt",
			UUIDFileName: "a.txt",
			FilePath:     fc.FilePath,
			Size:         1,
			SortOrder:    i,
		}}))
	}
	return fc
}

// countChildRows 统计子行数（含软删行）——「孤儿」的口径是物理存在的行。
func countChildRows(t *testing.T, repo *FileCodeRepository, fileCodeID uint) int64 {
	t.Helper()
	var n int64
	require.NoError(t, repo.db().Unscoped().Model(&model.FileCodeFile{}).
		Where("file_code_id = ?", fileCodeID).Count(&n).Error)
	return n
}

func TestHardDeleteByCode_RemovesChildRows(t *testing.T) {
	g := newGovernanceTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	uid := uint(7)
	fc := seedShareWithChildren(t, repo, ctx, "HD10001", uid, 2)
	require.Equal(t, int64(2), countChildRows(t, repo, fc.ID))

	// 硬删只认「已软删」的分享 ⇒ 先软删
	require.NoError(t, repo.DeleteByIDTx(ctx, fc.ID))

	n, err := repo.HardDeleteByCode(ctx, uid, "HD10001")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "应删掉 1 条主表行")

	assert.Equal(t, int64(0), countChildRows(t, repo, fc.ID),
		"硬删必须连带删掉 file_code_files 子行（core#62：否则留孤儿行 + 物理文件永不回收）")

	var parentLeft int64
	require.NoError(t, g.Unscoped().Model(&model.FileCode{}).
		Where("id = ?", fc.ID).Count(&parentLeft).Error)
	assert.Equal(t, int64(0), parentLeft, "主表行应已物理删除")
}

func TestHardDeleteByIDs_RemovesChildRows(t *testing.T) {
	g := newGovernanceTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	uid := uint(7)
	fc := seedShareWithChildren(t, repo, ctx, "HD10002", uid, 3)
	require.NoError(t, repo.DeleteByIDTx(ctx, fc.ID))

	n, err := repo.HardDeleteByIDs(ctx, []uint{fc.ID})
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	assert.Equal(t, int64(0), countChildRows(t, repo, fc.ID),
		"批量硬删也必须连带删子行")
}

// 负对照：硬删**未软删**的分享返回 0 行，此时子行必须原样保留
// （分享还活着，删子行会把它的文件清单掏空）。
func TestHardDeleteByCode_ActiveShare_KeepsChildRows(t *testing.T) {
	g := newGovernanceTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	uid := uint(7)
	fc := seedShareWithChildren(t, repo, ctx, "HD10003", uid, 2)

	n, err := repo.HardDeleteByCode(ctx, uid, "HD10003")
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "未软删的分享不得被硬删")
	assert.Equal(t, int64(2), countChildRows(t, repo, fc.ID),
		"未硬删的分享其子行必须保留（否则文件清单被掏空）")
}

// 负对照：硬删 A 不得误伤 B 的子行。
func TestHardDeleteByCode_KeepsOtherSharesChildren(t *testing.T) {
	g := newGovernanceTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	uid := uint(7)
	fcA := seedShareWithChildren(t, repo, ctx, "HD10004", uid, 2)
	fcB := seedShareWithChildren(t, repo, ctx, "HD10005", uid, 2)
	require.NoError(t, repo.DeleteByIDTx(ctx, fcA.ID))

	_, err := repo.HardDeleteByCode(ctx, uid, "HD10004")
	require.NoError(t, err)

	assert.Equal(t, int64(0), countChildRows(t, repo, fcA.ID))
	assert.Equal(t, int64(2), countChildRows(t, repo, fcB.ID),
		"不得误删别的分享的子行")
}

// 恢复必须把子行一起恢复（与 RestoreByIDs 同口径）。
//
// 背景：管理端批量删除会**软删子行**（app/admin/files.go）⇒ 用户在回收站
// 恢复该分享时，若只恢复主表 ⇒ 取件页清单为空、zip 无内容、逐文件下载 404
// （「恢复只恢复了一半」，2026-10-26 审计已在 RestoreByIDs 修过；
//
//	用户面 RestoreByCode 漏了同一口径 ⇒ 这里补齐）。
func TestRestoreByCode_RestoresChildRows(t *testing.T) {
	g := newGovernanceTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	uid := uint(7)
	fc := seedShareWithChildren(t, repo, ctx, "HD10006", uid, 2)

	// 主表 + 子表都软删（管理端批量删除的形状）
	require.NoError(t, repo.DeleteByIDTx(ctx, fc.ID))
	require.NoError(t, NewFileCodeFileRepository(repo.db()).SoftDeleteByFileCodeIDs(ctx, []uint{fc.ID}))

	n, err := repo.RestoreByCode(ctx, uid, "HD10006")
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	var alive int64
	require.NoError(t, repo.db().Model(&model.FileCodeFile{}).
		Where("file_code_id = ?", fc.ID).Count(&alive).Error)
	assert.Equal(t, int64(2), alive, "恢复必须连带恢复子行（否则取件页清单为空）")
}
