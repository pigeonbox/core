package share

import (
	"context"
	"testing"

	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHardDeleteUserShare_RemovesChildRows 端到端判据（core#62）：
// 用户在回收站点「彻底删除」后，file_code_files 不得残留孤儿子行。
//
// 判据覆盖 issue 正文给的两条验收：
//
//	① 软删 → 硬删后子表孤儿数 = 0；
//	② 负对照：未软删的分享硬删返回 0 行（ErrNotInRecycleBin）且子行原样保留。
func TestHardDeleteUserShare_RemovesChildRows(t *testing.T) {
	gormDB := newTestDB(t)
	svc := NewService(gormDB, "http://test", nil)
	ctx := context.Background()
	repo := dao.NewFileCodeRepository(gormDB)
	childRepo := dao.NewFileCodeFileRepository(gormDB)

	uid := uint(7)
	require.NoError(t, repo.Create(ctx, &model.FileCode{
		Code: "HD20001", UserID: &uid, FilePath: "uploads/2026/11/04/a.txt", Text: "a.txt",
	}))
	fc, err := repo.GetByCode(ctx, "HD20001")
	require.NoError(t, err)
	require.NoError(t, childRepo.CreateBatch(ctx, []*model.FileCodeFile{{
		FileCodeID: fc.ID, FileName: "a.txt", UUIDFileName: "a.txt",
		FilePath: "uploads/2026/11/04/a.txt",
	}}))

	// 软删 → 硬删
	n, err := repo.BatchSoftDeleteByCodes(ctx, uid, []string{"HD20001"})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.NoError(t, svc.HardDeleteUserShare(ctx, uid, "HD20001"))

	var orphan int64
	require.NoError(t, gormDB.Unscoped().Model(&model.FileCodeFile{}).
		Where("file_code_id = ?", fc.ID).Count(&orphan).Error)
	assert.Equal(t, int64(0), orphan, "彻底删除后不得残留孤儿子行（core#62）")
}

// 负对照：分享还活着时硬删必须显式失败，且子行不得被掏空。
func TestHardDeleteUserShare_ActiveShare_KeepsChildRows(t *testing.T) {
	gormDB := newTestDB(t)
	svc := NewService(gormDB, "http://test", nil)
	ctx := context.Background()
	repo := dao.NewFileCodeRepository(gormDB)
	childRepo := dao.NewFileCodeFileRepository(gormDB)

	uid := uint(7)
	require.NoError(t, repo.Create(ctx, &model.FileCode{
		Code: "HD20002", UserID: &uid, FilePath: "uploads/2026/11/04/b.txt", Text: "b.txt",
	}))
	fc, err := repo.GetByCode(ctx, "HD20002")
	require.NoError(t, err)
	require.NoError(t, childRepo.CreateBatch(ctx, []*model.FileCodeFile{{
		FileCodeID: fc.ID, FileName: "b.txt", UUIDFileName: "b.txt",
		FilePath: "uploads/2026/11/04/b.txt",
	}}))

	err = svc.HardDeleteUserShare(ctx, uid, "HD20002")
	require.Error(t, err, "活跃分享硬删必须显式报错（不在回收站）")

	var left int64
	require.NoError(t, gormDB.Unscoped().Model(&model.FileCodeFile{}).
		Where("file_code_id = ?", fc.ID).Count(&left).Error)
	assert.Equal(t, int64(1), left, "活跃分享的子行不得被删（否则文件清单被掏空）")
}
