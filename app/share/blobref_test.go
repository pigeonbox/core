package share

import (
	"context"
	"testing"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// newBlobRefTestService 同 newTestServiceWithDB，另建 blobs/file_references
// 两表（本包测试骨架 newTestDB 的 AutoMigrate 清单不含新表——骨架在
// service_test.go（#196 在途改动面），本文件不动它，自行补建）。
func newBlobRefTestService(t *testing.T) (*Service, *gorm.DB, *mockStorage) {
	t.Helper()
	svc, gormDB, st, _, _ := newTestServiceWithDB(t)
	require.NoError(t, gormDB.AutoMigrate(&model.Blob{}, &model.FileReference{}))
	return svc, gormDB, st
}

func blobRows(t *testing.T, g *gorm.DB) []model.Blob {
	t.Helper()
	var rows []model.Blob
	require.NoError(t, g.Find(&rows).Error)
	return rows
}

func refRows(t *testing.T, g *gorm.DB) []model.FileReference {
	t.Helper()
	var rows []model.FileReference
	require.NoError(t, g.Find(&rows).Error)
	return rows
}

// 写路径基本面：单文件 CreateShare ⇒ blob(active) + ref(active) 双写落账。
func TestCreateShare_WritesBlobAndRef(t *testing.T) {
	svc, g, _ := newBlobRefTestService(t)
	ctx := context.Background()
	u := uint(1)

	r, err := svc.CreateShare(ctx, dedupeReq(&u, "uploads/2026/11/16/a.bin", "hash-a", "a.bin", 10))
	require.NoError(t, err)

	blobs := blobRows(t, g)
	require.Len(t, blobs, 1, "应落 1 个 blob")
	assert.Equal(t, "uploads/2026/11/16/a.bin", blobs[0].ObjectKey)
	assert.Equal(t, model.BlobActive, blobs[0].State, "写路径建即 active")
	assert.Equal(t, int64(10), blobs[0].Size)

	refs := refRows(t, g)
	require.Len(t, refs, 1, "应落 1 条引用")
	assert.Equal(t, model.RefActive, refs[0].State)
	assert.Equal(t, blobs[0].ID, refs[0].BlobID)
	assert.Equal(t, r.ID, refs[0].FileCodeID, "引用应挂在新建分享上")
}

// 文本分享无物理对象 ⇒ 不落账。
func TestShareText_NoBlobRow(t *testing.T) {
	svc, g, _ := newBlobRefTestService(t)
	ctx := context.Background()

	_, err := svc.ShareText(ctx, &ShareTextReq{Text: "hello", ExpiredCount: -1})
	require.NoError(t, err)
	assert.Empty(t, blobRows(t, g), "文本分享不得落 blob")
	assert.Empty(t, refRows(t, g), "文本分享不得落引用")
}

// 硬删链（正例）：引用释放 + 归零 ⇒ blob 进 trashed（宽限期起点）。
func TestHardDeleteUserShare_ReleasesRefAndTrashesBlob(t *testing.T) {
	svc, g, _ := newBlobRefTestService(t)
	ctx := context.Background()
	u := uint(1)

	r, err := svc.CreateShare(ctx, dedupeReq(&u, "uploads/hd.bin", "hash-hd", "hd.bin", 10))
	require.NoError(t, err)
	require.Equal(t, 1, mustSoftDelete(t, svc, u, r.Code))

	require.NoError(t, svc.HardDeleteUserShare(ctx, u, r.Code))

	refs := refRows(t, g)
	require.Len(t, refs, 1)
	assert.Equal(t, model.RefReleased, refs[0].State, "硬删必须释放引用")
	require.NotNil(t, refs[0].ReleasedAt)

	blobs := blobRows(t, g)
	require.Len(t, blobs, 1)
	assert.Equal(t, model.BlobTrashed, blobs[0].State, "归零 ⇒ trashed（宽限期起点）")
	require.NotNil(t, blobs[0].TrashedAt, "trashed_at 必须落（宽限判据）")
}

// 负对照①（issue 验收）：软删不回收——软删后引用仍 active、blob 仍 active。
func TestSoftDelete_KeepsRefAndBlobActive(t *testing.T) {
	svc, g, _ := newBlobRefTestService(t)
	ctx := context.Background()
	u := uint(1)

	r, err := svc.CreateShare(ctx, dedupeReq(&u, "uploads/soft.bin", "hash-soft", "soft.bin", 10))
	require.NoError(t, err)
	require.Equal(t, 1, mustSoftDelete(t, svc, u, r.Code))

	refs := refRows(t, g)
	require.Len(t, refs, 1)
	assert.Equal(t, model.RefActive, refs[0].State, "负对照：软删不得释放引用（回收站行仍占对象）")

	blobs := blobRows(t, g)
	require.Len(t, blobs, 1)
	assert.Equal(t, model.BlobActive, blobs[0].State, "负对照：软删不得进回收态")

	// 恢复后照常可用（账本不动）
	require.NoError(t, svc.RestoreUserShare(ctx, u, r.Code))
	refs = refRows(t, g)
	assert.Equal(t, model.RefActive, refs[0].State)
}

// 负对照②（issue 验收）：引用未归零不回收——共享对象硬删其一，blob 保持 active。
func TestHardDelete_SharedObjectStaysActive(t *testing.T) {
	svc, g, _ := newBlobRefTestService(t)
	ctx := context.Background()
	u := uint(1)

	a, err := svc.CreateShare(ctx, dedupeReq(&u, "uploads/shared-src.bin", "hash-sh", "a.bin", 10))
	require.NoError(t, err)
	b, err := svc.CreateShare(ctx, dedupeReq(&u, "uploads/shared-copy.bin", "hash-sh", "b.bin", 10))
	require.NoError(t, err)
	require.True(t, b.Deduped, "同 hash 应秒传共享同一对象）")

	// 账本口径：两条引用挂同一 blob
	blobs := blobRows(t, g)
	require.Len(t, blobs, 1, "秒传共享 ⇒ 单 blob")
	refs := refRows(t, g)
	require.Len(t, refs, 2, "两条引用")
	assert.Equal(t, blobs[0].ID, refs[0].BlobID)
	assert.Equal(t, blobs[0].ID, refs[1].BlobID)

	// 硬删 A：B 仍引用 ⇒ blob 不得进回收态
	require.Equal(t, 1, mustSoftDelete(t, svc, u, a.Code))
	require.NoError(t, svc.HardDeleteUserShare(ctx, u, a.Code))

	blobs = blobRows(t, g)
	require.Len(t, blobs, 1)
	assert.Equal(t, model.BlobActive, blobs[0].State,
		"负对照②：引用未归零（B 仍引用）不得进回收态")

	var alive int64
	require.NoError(t, g.Model(&model.FileReference{}).
		Where("blob_id = ? AND state = ?", blobs[0].ID, model.RefActive).Count(&alive).Error)
	assert.Equal(t, int64(1), alive, "B 的引用应仍 active")

	// 硬删 B ⇒ 归零进 trashed
	require.Equal(t, 1, mustSoftDelete(t, svc, u, b.Code))
	require.NoError(t, svc.HardDeleteUserShare(ctx, u, b.Code))
	blobs = blobRows(t, g)
	assert.Equal(t, model.BlobTrashed, blobs[0].State, "全部硬删后才进回收态")
}

// 多文件分享：逐子路径落账（首子行与主行同路径 ⇒ 幂等单行）。
func TestCreateMultiFileShare_WritesRefsPerChild(t *testing.T) {
	svc, g, _ := newBlobRefTestService(t)
	ctx := context.Background()
	u := uint(1)

	_, err := svc.CreateMultiFileShare(ctx, &MultiShareReq{
		UserID: &u, ExpiredCount: -1, UploadType: "authenticated",
		Entries: []StoredFileEntry{{
			RelPath: "uploads/m1/a.txt", FileName: "a.txt", Size: 3, FileHash: "h-a",
		}, {
			RelPath: "uploads/m1/b.txt", FileName: "b.txt", Size: 4, FileHash: "h-b",
		}},
	})
	require.NoError(t, err)

	blobs := blobRows(t, g)
	assert.Len(t, blobs, 2, "两个子文件 ⇒ 两个 blob（同分享不同路径）")
	refs := refRows(t, g)
	assert.Len(t, refs, 2, "两条引用（主行与首子行同路径 ⇒ 幂等不重复）")
	for _, rf := range refs {
		assert.Equal(t, model.RefActive, rf.State)
	}
}

// 配额守卫（core#172，沿用 core#169 预留账本口径不重复实现）：
// 配额闸拒绝 ⇒ 分享未建 ⇒ 账本零残留（blob/引用行都不得出现）——
// 即账本双写挂在 reserveUploadCaps 之后的 insert 成功路径上，
// 不绕过、不提前于 #169 的预留-提交-回滚账本。
func TestCreateShare_QuotaRejectedLeavesNoLedgerRows(t *testing.T) {
	svc, g, _ := newBlobRefTestService(t)
	ctx := context.Background()

	// 存量 800 + 配额 1000（setStorageQuota 口径来自 globalquota_test.go，
	// 即 #169 的测试口径）⇒ 新 300 必拒
	require.NoError(t, g.Create(&model.FileCode{
		Code: "QG0001", FilePath: "uploads/qg.bin", Size: 800, Text: "qg", ExpiredCount: -1,
	}).Error)
	setStorageQuota(t, 1000)
	u := uint(1)

	_, err := svc.CreateShare(ctx, dedupeReq(&u, "uploads/qg-new.bin", "h-qg", "n.bin", 300))
	require.Error(t, err, "超额必须被 #169 预留账本拒绝")

	assert.Empty(t, blobRows(t, g), "配额拒绝 ⇒ 不得落 blob 行")
	assert.Empty(t, refRows(t, g), "配额拒绝 ⇒ 不得落引用行")
}

// mustSoftDelete 回收站软删（返回行数）。
func mustSoftDelete(t *testing.T, svc *Service, userID uint, code string) int {
	t.Helper()
	n, err := svc.fileCodeRepo.BatchSoftDeleteByCodes(context.Background(), userID, []string{code})
	require.NoError(t, err)
	return n
}
