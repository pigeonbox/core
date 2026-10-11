package chunk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/model"
)

// 大文件分片生命周期回归（对标上游 issue 区最高频踩坑：大文件上传失败/卡 0%、
// 断点续传、分片幂等）。以真实量级参数（25MB = 5MB×5 分片）驱动
// init → 部分上传（模拟断线）→ 断点查询 → 幂等重传 → 补齐完成 → 秒传命中 全链路。
func TestLargeFileChunkLifecycle(t *testing.T) {
	gormDB := newTestDB(t)
	// 秒传检索查 file_codes，补迁移
	require.NoError(t, gormDB.AutoMigrate(&model.FileCode{}))

	svc := NewService(gormDB)
	ctx := context.Background()

	const totalChunks = 5
	const chunkSize = 5 * 1024 * 1024 // 5MB（S3 单分片下限量级）
	const fileSize = totalChunks * chunkSize

	chunkHash := func(i int) string {
		h := sha256.Sum256([]byte(fmt.Sprintf("chunk-%d-bytes", i)))
		return hex.EncodeToString(h[:])
	}

	// 1. init（文件名带路径穿越尝试，验证消毒）
	resp, err := svc.InitiateUpload(ctx, &InitiateUploadReq{
		UploadID:    "lf-001",
		FileName:    "../../movie-25mb.mp4",
		TotalChunks: totalChunks,
		FileSize:    fileSize,
		ChunkSize:   chunkSize,
		OwnerIP:     "10.0.0.9",
	})
	require.NoError(t, err)
	require.Equal(t, totalChunks, resp.TotalChunks)

	// 2. 上传 0,1,2 后「断线」
	for i := 0; i < 3; i++ {
		_, err := svc.UploadChunk(ctx, &UploadChunkReq{
			UploadID: "lf-001", ChunkIndex: i, ChunkHash: chunkHash(i), ChunkSize: chunkSize,
		})
		require.NoError(t, err)
	}

	// 3. 断点续传查询：恰好 [0,1,2]
	idx, err := svc.GetUploadedChunkIndexes(ctx, "lf-001")
	require.NoError(t, err)
	assert.ElementsMatch(t, []int{0, 1, 2}, idx)

	progress, err := svc.CheckUploadProgress(ctx, "lf-001")
	require.NoError(t, err)
	assert.Equal(t, int64(3), progress.CompletedChunks)
	assert.InDelta(t, 60.0, progress.Progress, 0.01)

	// 4. 续传：分片 1 重传必须幂等（不重复计数），再补 3,4
	_, err = svc.UploadChunk(ctx, &UploadChunkReq{
		UploadID: "lf-001", ChunkIndex: 1, ChunkHash: chunkHash(1), ChunkSize: chunkSize,
	})
	require.NoError(t, err)
	for i := 3; i < totalChunks; i++ {
		_, err := svc.UploadChunk(ctx, &UploadChunkReq{
			UploadID: "lf-001", ChunkIndex: i, ChunkHash: chunkHash(i), ChunkSize: chunkSize,
		})
		require.NoError(t, err)
	}
	progress, err = svc.CheckUploadProgress(ctx, "lf-001")
	require.NoError(t, err)
	assert.Equal(t, int64(totalChunks), progress.CompletedChunks)

	// 5. 完成会话
	require.NoError(t, svc.CompleteUpload(ctx, "lf-001"))

	// 6. 非法索引拒绝（越界）
	_, err = svc.UploadChunk(ctx, &UploadChunkReq{
		UploadID: "lf-001", ChunkIndex: totalChunks, ChunkHash: "x", ChunkSize: chunkSize,
	})
	require.Error(t, err)

	// 7. 跨会话秒传：插入同 hash+size 的既有分享后命中
	fc := &model.FileCode{
		Code: "LF9999", FilePath: "uploads/2026/10/03/x.mp4", Size: fileSize,
		FileHash: chunkHash(0), Text: "movie-25mb.mp4",
		ExpiredCount: -1, // -1 = 无限次（0 语义是"已耗尽"）
	}
	require.NoError(t, gormDB.Create(fc).Error)
	code, err := svc.CheckQuickUpload(ctx, chunkHash(0), fileSize)
	require.NoError(t, err)
	assert.Equal(t, "LF9999", code)
}
