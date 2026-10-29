package chunk

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/model"
)

// TestInitiateUpload_CompletedSessionNotBlockedForOtherIP 回归（2026-10-30 真机实录）：
//
// upload_id 由 file_hash 派生 ⇒ **同一文件永远复用同一 upload_id**；而已完成会话在
// upload_chunks 里**留到 7 天后**才被 janitor 清理（DeleteStaleSessions(completed=true)，
// 见 app/admin/janitor.go）⇒ 同一文件在**换一个来源 IP**（换设备/换网络/换反代出口/
// 手机切基站）后重传 ⇒ 撞"他人进行中的会话"⇒ 409「该文件已有进行中的上传会话，
// 请稍后重试或更换文件」⇒ 用户**7 天内**无法重传同一文件。
//
// 而且文案与事实不符：该会话**已完成**（分片早已合并成文件），没有任何"进行中"的
// 数据可被截胡 ⇒ 防截胡闸不该对它生效。
//
// 期望：completed 会话不触发防截胡闸 ⇒ init 成功，且会话被**重置**（进度归零，
// 本次上传从头来）。
func TestInitiateUpload_CompletedSessionNotBlockedForOtherIP(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	const uid = "a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4"

	// 1) 机器 A 上传并完成（控制行 status=completed）
	_, err := svc.InitiateUpload(ctx, &InitiateUploadReq{
		UploadID: uid, FileName: "movie.mp4", TotalChunks: 2, FileSize: 20, ChunkSize: 10, OwnerIP: "10.0.0.1",
	})
	require.NoError(t, err)
	require.NoError(t, svc.chunkRepo.UpsertChunk(ctx, &model.UploadChunk{
		UploadID: uid, ChunkIndex: 0, ChunkHash: "h0", Status: "completed", Completed: true,
	}))
	require.NoError(t, svc.chunkRepo.UpdateChunkCompleted(ctx, uid, -1, "")) // 控制行置 completed

	// 2) 机器 B（不同 IP）重传同一文件 ⇒ 不得 409
	resp, err := svc.InitiateUpload(ctx, &InitiateUploadReq{
		UploadID: uid, FileName: "movie.mp4", TotalChunks: 2, FileSize: 20, ChunkSize: 10, OwnerIP: "10.0.0.2",
	})
	require.NoError(t, err, "已完成会话不应按『他人进行中』拒绝（换 IP 重传同一文件）")
	assert.Equal(t, uid, resp.UploadID)

	// 3) 会话已被重置：状态回 pending、进度归零（客户端须重传分片）
	live, err := svc.chunkRepo.GetByUploadID(ctx, uid)
	require.NoError(t, err)
	assert.Equal(t, "pending", live.Status, "重置后控制行状态应回 pending")
	assert.False(t, live.Completed, "重置后已完成分片数应归零")

	indexes, err := svc.GetUploadedChunkIndexes(ctx, uid)
	require.NoError(t, err)
	assert.Empty(t, indexes, "重置后已完成分片索引应归零（客户端重传）")

	// 4) 重置后归属跟着本次请求走（否则下一轮又会被判"他人"）
	assert.Equal(t, "10.0.0.2", live.OwnerIP)
}

// TestInitiateUpload_UnfinishedSessionStillConflicts 负对照：防截胡闸**不能**被
// 削弱——**未完成**（进行中）且已含分片的他人会话，换 IP 来 init 仍必须 409。
func TestInitiateUpload_UnfinishedSessionStillConflicts(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	const uid = "b1b2c3d4b1b2c3d4b1b2c3d4b1b2c3d4"

	_, err := svc.InitiateUpload(ctx, &InitiateUploadReq{
		UploadID: uid, FileName: "movie.mp4", TotalChunks: 2, FileSize: 20, ChunkSize: 10, OwnerIP: "10.0.0.1",
	})
	require.NoError(t, err)
	require.NoError(t, svc.chunkRepo.UpsertChunk(ctx, &model.UploadChunk{
		UploadID: uid, ChunkIndex: 0, ChunkHash: "h0", Status: "completed", Completed: true,
	}))

	// 控制行仍是 pending（未完成）⇒ 他人来 init 必须被拒
	_, err = svc.InitiateUpload(ctx, &InitiateUploadReq{
		UploadID: uid, FileName: "movie.mp4", TotalChunks: 2, FileSize: 20, ChunkSize: 10, OwnerIP: "10.0.0.2",
	})
	assert.ErrorIs(t, err, ErrSessionConflict, "进行中的他人会话仍须 409（防截胡闸不得削弱）")
}

// TestInitiateUpload_CompletedSessionSameOwnerIdempotent 同属主的已完成会话仍走
// 幂等返回（秒传命中语义不变）——本次修复不得动这一条。
func TestInitiateUpload_CompletedSessionSameOwnerIdempotent(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	const uid = "c1b2c3d4c1b2c3d4c1b2c3d4c1b2c3d4"

	_, err := svc.InitiateUpload(ctx, &InitiateUploadReq{
		UploadID: uid, FileName: "movie.mp4", TotalChunks: 2, FileSize: 20, ChunkSize: 10, OwnerIP: "10.0.0.1",
	})
	require.NoError(t, err)
	require.NoError(t, svc.chunkRepo.UpsertChunk(ctx, &model.UploadChunk{
		UploadID: uid, ChunkIndex: 0, ChunkHash: "h0", Status: "completed", Completed: true,
	}))
	require.NoError(t, svc.chunkRepo.UpdateChunkCompleted(ctx, uid, -1, ""))

	// 同 IP 再 init ⇒ 幂等返回既有会话（completed），**不**被重置
	resp, err := svc.InitiateUpload(ctx, &InitiateUploadReq{
		UploadID: uid, FileName: "movie.mp4", TotalChunks: 2, FileSize: 20, ChunkSize: 10, OwnerIP: "10.0.0.1",
	})
	require.NoError(t, err)
	assert.Equal(t, "completed", resp.Status, "同属主的已完成会话应幂等返回 completed")

	live, err := svc.chunkRepo.GetByUploadID(ctx, uid)
	require.NoError(t, err)
	assert.Equal(t, "completed", live.Status, "同属主路径不得被重置")
}
