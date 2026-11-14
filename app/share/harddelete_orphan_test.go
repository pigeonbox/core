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

// 评审 D03①：用户彻底删除后，无其他引用的对象必须被清掉（与 admin 删除同编排）。
// 此前 HardDeleteUserShare 只删 DB 行、对象永不删 ⇒ 只能等 janitor 兜底。
func TestHardDeleteUserShare_RemovesUnreferencedObject(t *testing.T) {
	svc, _, st, _, _ := newTestServiceWithDB(t)
	ctx := context.Background()
	u := uint(1)
	const objPath = "uploads/2026/11/14/hd-aaaa-bbbb.bin"

	a, err := svc.CreateShare(ctx, dedupeReq(&u, objPath, "hd-hash-1", "file.bin", 10))
	require.NoError(t, err)

	// 进回收站（硬删的前置态）
	n, err := svc.fileCodeRepo.BatchSoftDeleteByCodes(ctx, u, []string{a.Code})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	require.NoError(t, svc.HardDeleteUserShare(ctx, u, a.Code))
	assert.Contains(t, st.deletedPaths, objPath, "无引用对象硬删后必须清除（评审 D03①）")
}

// 评审 D03②：秒传共享对象——另一分享仍引用时绝不误删（宁泄漏勿误删）；
// 引用清零（两条分享都硬删）后才释放物理对象。
func TestHardDeleteUserShare_KeepsSharedObject(t *testing.T) {
	svc, _, st, _, _ := newTestServiceWithDB(t)
	ctx := context.Background()
	u := uint(1)

	a, err := svc.CreateShare(ctx, dedupeReq(&u, "uploads/2026/11/14/shared-src.bin", "hd-hash-2", "a.bin", 10))
	require.NoError(t, err)
	b, err := svc.CreateShare(ctx, dedupeReq(&u, "uploads/2026/11/14/shared-copy.bin", "hd-hash-2", "b.bin", 10))
	require.NoError(t, err)
	require.True(t, b.Deduped, "同 hash 应秒传共享同一对象")
	require.Equal(t, a.FilePath, b.FilePath, "两条分享应指向同一物理对象")
	shared := a.FilePath

	// 硬删 A：B 仍引用共享对象 → 物理对象保留
	n, err := svc.fileCodeRepo.BatchSoftDeleteByCodes(ctx, u, []string{a.Code})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.NoError(t, svc.HardDeleteUserShare(ctx, u, a.Code))
	assert.NotContains(t, st.deletedPaths, shared,
		"共享对象被另一分享引用时绝不得删（评审 D03②，宁泄漏勿误删）")

	// B 也进回收站并硬删 → 引用清零 → 对象释放
	n, err = svc.fileCodeRepo.BatchSoftDeleteByCodes(ctx, u, []string{b.Code})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.NoError(t, svc.HardDeleteUserShare(ctx, u, b.Code))
	assert.Contains(t, st.deletedPaths, shared, "引用清零后共享对象应释放")
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
