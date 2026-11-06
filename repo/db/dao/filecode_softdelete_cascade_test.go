package dao

import (
	"context"
	"testing"
	"time"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── 软删必须带走子表行（与硬删/恢复同口径「同生共死」）────────────────────
//
// file_code_files 子表是「文件清单的唯一真相源」（v0.32.14 起），与主表同生共死：
// 硬删带走子行（core#62）、恢复带走子行（2026-10-26 审计），但**软删**此前只有
// 两条路径级联（admin.BatchDeleteFiles / CleanExpiredFiles，且都以 storage!=nil
// 为前提，app 层散落 4 处各自补刀），用户面 BatchSoftDeleteByCodes（回收站入口）
// 与 DeleteByIDTx 完全不级联 ⇒ 同一个「软删」状态出现两种子行状态：
//   · 管理端删：父软删 + 子软删；
//   · 用户端删：父软删 + 子**活**。
// 后果：任何按「父软删 ⇒ 子软删」假设写的代码在用户路径上遇到反例；
// 且 storage==nil 时连管理端也不级联（级联被绑在物理清理分支里）。
//
// ⇒ 级联统一收进 dao（与 hardDeleteChildRows 同层），所有父行软删路径同一事务
// 连带软删子行；app 层散落的 SoftDeleteByFileCodeIDs 补刀全部退役。
//
// 负对照（本文件断言的牙齿）：把任一方法里的 softDeleteChildRows 调用去掉，
// 对应用例即转红（子行仍活）。

// countAliveChildRows 子行数（默认作用域 = 只算活着的）。
func countAliveChildRows(t *testing.T, repo *FileCodeRepository, fileCodeID uint) int64 {
	t.Helper()
	var n int64
	require.NoError(t, repo.db().Model(&model.FileCodeFile{}).
		Where("file_code_id = ?", fileCodeID).Count(&n).Error)
	return n
}

func TestDeleteByIDTx_SoftDeletesChildRows(t *testing.T) {
	g := newGovernanceTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	fc := seedShareWithChildren(t, repo, ctx, "SD10001", 7, 2)
	require.Equal(t, int64(2), countAliveChildRows(t, repo, fc.ID))

	require.NoError(t, repo.DeleteByIDTx(ctx, fc.ID))

	assert.Equal(t, int64(0), countAliveChildRows(t, repo, fc.ID),
		"单条软删必须连带软删子行（同生共死）")
	assert.Equal(t, int64(2), countChildRows(t, repo, fc.ID),
		"软删不是物理删除：子行还在（回收站可恢复）")
}

func TestBatchSoftDeleteByCodes_SoftDeletesChildRows(t *testing.T) {
	g := newGovernanceTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	uid := uint(7)
	fc := seedShareWithChildren(t, repo, ctx, "SD10002", uid, 3)

	n, err := repo.BatchSoftDeleteByCodes(ctx, uid, []string{"SD10002"})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	assert.Equal(t, int64(0), countAliveChildRows(t, repo, fc.ID),
		"用户面回收站入口（BatchSoftDeleteByCodes）必须连带软删子行")
}

// 负对照：软删 A 不得误伤 B 的子行（B 还活着，其文件清单不能被掏空）。
func TestBatchSoftDeleteByCodes_KeepsOtherSharesChildren(t *testing.T) {
	g := newGovernanceTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	uid := uint(7)
	fcA := seedShareWithChildren(t, repo, ctx, "SD10003", uid, 2)
	fcB := seedShareWithChildren(t, repo, ctx, "SD10004", uid, 2)

	_, err := repo.BatchSoftDeleteByCodes(ctx, uid, []string{"SD10003"})
	require.NoError(t, err)

	assert.Equal(t, int64(0), countAliveChildRows(t, repo, fcA.ID))
	assert.Equal(t, int64(2), countAliveChildRows(t, repo, fcB.ID),
		"不得误删别的分享的子行")
}

// 负对照：BatchSoftDeleteByCodes 限定 userID —— 别的用户的同名 code 不得被删。
func TestBatchSoftDeleteByCodes_ScopedToUser(t *testing.T) {
	g := newGovernanceTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	fcA := seedShareWithChildren(t, repo, ctx, "SD10005", 7, 2)
	fcB := seedShareWithChildren(t, repo, ctx, "SD10006", 8, 2)

	// 用户 8 试图删用户 7 的 code ⇒ 必须 0 行命中（user_id 限定）
	n, err := repo.BatchSoftDeleteByCodes(ctx, 8, []string{"SD10005"})
	require.NoError(t, err)
	assert.Equal(t, 0, n, "user_id 限定：不得删到别的用户的分享")
	assert.Equal(t, int64(2), countAliveChildRows(t, repo, fcA.ID),
		"别的用户的分享子行不得被波及")

	// 用户 7 删自己的 ⇒ 只动自己
	_, err = repo.BatchSoftDeleteByCodes(ctx, 7, []string{"SD10005"})
	require.NoError(t, err)
	assert.Equal(t, int64(0), countAliveChildRows(t, repo, fcA.ID))
	assert.Equal(t, int64(2), countAliveChildRows(t, repo, fcB.ID))
}

func TestBatchDeleteByIDs_SoftDeletesChildRows(t *testing.T) {
	g := newGovernanceTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	fc := seedShareWithChildren(t, repo, ctx, "SD10007", 7, 2)

	n, err := repo.BatchDeleteByIDs(ctx, []uint{fc.ID})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	assert.Equal(t, int64(0), countAliveChildRows(t, repo, fc.ID),
		"管理端批量软删必须连带软删子行（此前只有 app 层在 storage!=nil 时补刀）")
}

func TestDeleteExpiredFiles_SoftDeletesChildRows(t *testing.T) {
	g := newGovernanceTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	expired := time.Now().Add(-time.Hour)
	fc := &model.FileCode{Code: "SD10008", FilePath: "uploads/SD10008/a.txt", ExpiredAt: &expired}
	require.NoError(t, repo.Create(ctx, fc))
	childRepo := NewFileCodeFileRepository(repo.db())
	require.NoError(t, childRepo.CreateBatch(ctx, []*model.FileCodeFile{
		{FileCodeID: fc.ID, FileName: "a.txt", FilePath: "uploads/SD10008/a.txt", Size: 1},
	}))

	rows, err := repo.GetExpiredFiles(ctx)
	require.NoError(t, err)
	n, err := repo.DeleteExpiredFiles(ctx, rows)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	assert.Equal(t, int64(0), countAliveChildRows(t, repo, fc.ID),
		"过期清理软删父行也必须连带软删子行")
}
