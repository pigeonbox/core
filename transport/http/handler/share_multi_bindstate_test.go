// bindChunkEntry 的账本挂接（core#172 第三片·拆分项 3，multi 合并面）：
//
//	成功：合并+复核通过 ⇒ committed 落行（此面不建分享，停在 committed 由
//	      调用方后续 CreateMultiFileShare 推进 active——与 single 面分工一致）
//	拒收：扩展名黑名单 ⇒ 对象已删、账本 aborted
//
// 负对照：抽掉 chunkSvc.BlobEnsureCommitted ⇒ 两用例都拿不到行（Abort 只推进
// 不建行）双红；抽掉 BlobAbort ⇒ 拒用例停在 committed 红。
package handler

import (
	"context"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	chunkService "github.com/pigeonbox/core/app/chunk"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/pigeonbox/core/storage"
)

func newBindStateEnv(t *testing.T) (*gorm.DB, *chunkService.Service, storage.StorageInterface) {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.UploadChunk{}, &model.Blob{}, &model.FileReference{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	t.Setenv("PB_PRESIGN_SIGNING_KEY", "test-presign-signing-key-0123456789")

	st, err := storage.NewStorageServiceE(&storage.StorageConfig{
		Type:     storage.StorageTypeLocal,
		DataPath: t.TempDir(),
	})
	require.NoError(t, err)

	cs := chunkService.NewService(g)
	cs.SetStorage(st)
	return g, cs, st
}

func seedBindSession(t *testing.T, g *gorm.DB, cs *chunkService.Service, uploadID, fileName string, data []byte) {
	t.Helper()
	require.NoError(t, g.Create(&model.UploadChunk{
		UploadID: uploadID, ChunkIndex: -1, FileName: fileName,
		FileSize: int64(len(data)), TotalChunks: 1, ChunkSize: len(data),
		OwnerIP: "0.0.0.0", Status: "uploading",
	}).Error)
	require.NoError(t, g.Create(&model.UploadChunk{
		UploadID: uploadID, ChunkIndex: 0, FileName: fileName,
		FileSize: int64(len(data)), TotalChunks: 1, ChunkSize: len(data),
		Completed: true, Status: "completed",
	}).Error)
	require.NoError(t, cs.SaveChunkData(t.Context(), uploadID, 0, data))
}

func newBindCtx(uploadID string) *app.RequestContext {
	c := app.NewContext(0)
	c.Request.Header.Set("X-Upload-Token", chunkService.SessionToken(uploadID))
	return c
}

// TestBindChunkEntry_CommittedLedger 成功：复核通过 ⇒ committed 落行（size/hash
// 是合并实测值）；分享未建，不越权推进 active。
func TestBindChunkEntry_CommittedLedger(t *testing.T) {
	g, cs, st := newBindStateEnv(t)
	const uploadID = "BINDSTATE-A1"
	data := []byte("hello multi bind ledger")
	seedBindSession(t, g, cs, uploadID, "hello.txt", data)

	entry, _, ok := bindChunkEntry(context.Background(), newBindCtx(uploadID), cs, st, uploadID, false)
	require.True(t, ok, "绑定应成功")

	var blobs []model.Blob
	require.NoError(t, g.Find(&blobs).Error)
	require.Len(t, blobs, 1, "绑定成功必须落一条账本行")
	assert.Equal(t, model.BlobCommitted, blobs[0].State, "此面不建分享 ⇒ 应停在 committed")
	assert.Equal(t, int64(len(data)), blobs[0].Size, "size 应是合并实测值")
	assert.NotEmpty(t, blobs[0].FileHash, "fileHash 应是合并实测哈希")
	assert.Equal(t, entry.RelPath, blobs[0].ObjectKey, "账本 key 必须是绑定返回的对象路径")
}

// TestBindChunkEntry_AbortedOnReject 拒收（负对照）：黑名单扩展名 ⇒ 对象已删、
// 账本 aborted；同时证明 committed 落行发生过（Abort 只推进不建行）。
func TestBindChunkEntry_AbortedOnReject(t *testing.T) {
	g, cs, st := newBindStateEnv(t)
	const uploadID = "BINDSTATE-B1"
	data := []byte("MZ payload")
	seedBindSession(t, g, cs, uploadID, "evil.exe", data)

	_, _, ok := bindChunkEntry(context.Background(), newBindCtx(uploadID), cs, st, uploadID, false)
	require.False(t, ok, "黑名单扩展名应被拒")

	var blobs []model.Blob
	require.NoError(t, g.Find(&blobs).Error)
	require.Len(t, blobs, 1, "拒收链应留下账本行")
	assert.Equal(t, model.BlobAborted, blobs[0].State, "对象已删 ⇒ 必须 aborted（停 committed = BlobAbort 没接）")
}
