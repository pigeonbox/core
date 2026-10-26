package chunk

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/model"
)

// TestInitiateUpload_ResurrectSoftDeletedSession 回归（2026-10-26 真机冒烟
// S11c/S11d 假红 → 真缺陷）：
//
// upload_id 由 file_hash 派生（客户端不带 upload_id 时服务端拿哈希当会话键）⇒
// **同一文件重传会复用同一 upload_id**；会话清理（DeleteStaleSessions）对控制行
// 与分片行做的是**软删** ⇒ 默认作用域的 GetByUploadID 查不到 ⇒ init 若走「新建」
// ⇒ 撞 (upload_id, chunk_index) 唯一索引（迁移 000006 的索引**不区分软删行**）
// ⇒ init 直接 10008「初始化上传失败」⇒ **同一文件在清理窗口过后必然无法分片上传**。
//
// 期望：旧会话被清理（软删）后，同 upload_id 的 init 必须**复活**它并成功返回。
func TestInitiateUpload_ResurrectSoftDeletedSession(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	const uid = "deadbeefdeadbeefdeadbeefdeadbeef"

	// 1) 正常 init ⇒ 建控制行
	resp, err := svc.InitiateUpload(ctx, &InitiateUploadReq{
		UploadID:    uid,
		FileName:    "same.bin",
		TotalChunks: 2,
		FileSize:    20,
		ChunkSize:   10,
		OwnerIP:     "10.0.0.9",
	})
	require.NoError(t, err)
	require.Equal(t, uid, resp.UploadID)

	// 2) 模拟会话清理：控制行 + 分片行一起软删（DeleteStaleSessions 的形态）
	require.NoError(t, svc.chunkRepo.UpsertChunk(ctx, &model.UploadChunk{
		UploadID:   uid,
		ChunkIndex: 0,
		ChunkHash:  "deadbeef",
		Status:     "completed",
		Completed:  true,
	}))
	require.NoError(t, svc.chunkRepo.DeleteByUploadID(ctx, uid))

	// 软删确认（默认作用域查不到，Unscoped 查得到）
	_, err = svc.chunkRepo.GetByUploadID(ctx, uid)
	require.Error(t, err, "清理后默认作用域应查不到")
	stale, err := svc.chunkRepo.GetByUploadIDUnscoped(ctx, uid)
	require.NoError(t, err, "清理后 Unscoped 应能查到（软删行仍在）")
	require.NotNil(t, stale.DeletedAt)

	// 3) 同一 upload_id 再次 init（= 同一文件重传）⇒ 必须成功（复活），不能撞唯一索引
	resp2, err := svc.InitiateUpload(ctx, &InitiateUploadReq{
		UploadID:    uid,
		FileName:    "same.bin",
		TotalChunks: 2,
		FileSize:    20,
		ChunkSize:   10,
		OwnerIP:     "10.0.0.9",
	})
	require.NoError(t, err, "旧会话被清理后，同 upload_id 的 init 必须复活会话而不是撞唯一索引")
	assert.Equal(t, uid, resp2.UploadID)

	// 4) 复活后的控制行是活的，且状态回 pending（客户端需重传分片）
	live, err := svc.chunkRepo.GetByUploadID(ctx, uid)
	require.NoError(t, err)
	assert.Equal(t, "pending", live.Status)
	assert.False(t, live.Completed)

	// 5) 复活后分片行也是活的（UpsertChunk 的 UPDATE 才能命中 ⇒ 分片能记上）
	//    且完成标记被置回 false（磁盘分片可能已回收 ⇒ 客户端必须重传）
	indexes, err := svc.chunkRepo.GetUploadedChunkIndexes(ctx, uid)
	require.NoError(t, err)
	assert.Empty(t, indexes, "复活的分片行须置回未完成（已完成数应归零）")

	chunkRows, err := svc.chunkRepo.ListChunksUnscoped(ctx, uid)
	require.NoError(t, err)
	require.Len(t, chunkRows, 1, "分片行应被复活（仍在表里）")
	assert.False(t, chunkRows[0].DeletedAt.Valid, "分片行应被复活为活行")
	assert.False(t, chunkRows[0].Completed)
}

// TestInitiateUpload_ResurrectKeepsOtherOwnerRejected 回归：他人**已有分片**的
// 会话不得被认领（复活路径同样要过归属闸）。
func TestInitiateUpload_ResurrectKeepsOtherOwnerRejected(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	const uid = "cafebabecafebabecafebabecafebabe"

	_, err := svc.InitiateUpload(ctx, &InitiateUploadReq{
		UploadID:    uid,
		FileName:    "other.bin",
		TotalChunks: 2,
		FileSize:    20,
		ChunkSize:   10,
		OwnerIP:     "10.0.0.1",
	})
	require.NoError(t, err)

	// 他人已传分片（活的）⇒ 会话非空
	require.NoError(t, svc.chunkRepo.UpsertChunk(ctx, &model.UploadChunk{
		UploadID:   uid,
		ChunkIndex: 0,
		ChunkHash:  "deadbeef",
		Status:     "completed",
		Completed:  true,
	}))

	// 另一 IP 来 init ⇒ 会话冲突（既不能新建也不能复活认领）
	_, err = svc.InitiateUpload(ctx, &InitiateUploadReq{
		UploadID:    uid,
		FileName:    "other.bin",
		TotalChunks: 2,
		FileSize:    20,
		ChunkSize:   10,
		OwnerIP:     "10.0.0.2",
	})
	assert.ErrorIs(t, err, ErrSessionConflict)
}
