// 分片行的唯一性（2026-10-25 审计 A3）
//
// 缺陷：`UploadChunk` 无 (upload_id, chunk_index) 唯一索引，且
// `dao.GetChunkByIndex` 的 WHERE 含 `completed = true` ⇒ **未完成的旧行查不到**
// ⇒ 分片失败后重传同索引会**再建一行**（service 里的 Update 分支实为死代码）
// ⇒ `dao.CountCompletedChunks` 按**行数**计 ⇒ 「分片齐全」判定可虚高
// （声明 5 片只传 4 片也可能被计成 5 ⇒ 合并阶段才发现缺片）。
//
// 修法：① 写入改用幂等原语 `dao.FirstOrCreateChunk`（按 (upload_id, chunk_index)
// 命中即更新）；② (upload_id, chunk_index) 唯一索引（迁移 000006 + 模型标签）。
//
// 断言必须能变红（负对照）：把写入改回「查不到就 Create」⇒ 本用例红。
package chunk

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
)

// TestUploadChunk_RetryAfterFailure_NoDuplicateRow 失败后重传同索引 ⇒ **不得**
// 新建第二行（否则计数按行数 ⇒ 「分片齐全」虚高）。
func TestUploadChunk_RetryAfterFailure_NoDuplicateRow(t *testing.T) {
	gormDB := newTestDB(t)
	svc := NewService(gormDB)
	ctx := context.Background()

	_, err := svc.InitiateUpload(ctx, &InitiateUploadReq{
		UploadID: "u-retry", TotalChunks: 2, FileName: "f.bin",
	})
	require.NoError(t, err)

	// 模拟「上一次传输失败留下的未完成行」（completed=false ⇒ GetChunkByIndex 查不到）
	repo := dao.NewChunkRepository(gormDB)
	require.NoError(t, repo.Create(ctx, &model.UploadChunk{
		UploadID:   "u-retry",
		ChunkIndex: 0,
		ChunkHash:  "stale-hash",
		ChunkSize:  10,
		Status:     "uploading",
		Completed:  false,
	}))

	// 重传同索引 ⇒ 应命中既有行并更新为 completed
	resp, err := svc.UploadChunk(ctx, &UploadChunkReq{
		UploadID:   "u-retry",
		ChunkIndex: 0,
		ChunkHash:  "fresh-hash",
		ChunkSize:  10,
	})
	require.NoError(t, err)
	assert.True(t, resp.Completed)

	// 关键：同一 (upload_id, chunk_index) **只有一行**
	var rows int64
	require.NoError(t, gormDB.Model(&model.UploadChunk{}).
		Where("upload_id = ? AND chunk_index = ?", "u-retry", 0).Count(&rows).Error)
	assert.Equal(t, int64(1), rows, "重传同索引不得新建第二行（修前会 ⇒ 计数虚高）")
}

// TestUploadChunk_TwoSessionsSameIndexCoexist 回归（2026-10-25 CI 真机冒烟实测）：
//
// 唯一索引若只挂在 `ChunkIndex` 单列上（gorm 的 `uniqueIndex:` 标签只加在一个
// 字段 ⇒ 建的是**单列**索引），则**不同上传会话的同索引分片互相冲突**
// ⇒ 「UNIQUE constraint failed: upload_chunks.chunk_index」
// ⇒ 秒传/多会话上传链路整条挂掉（CI 冒烟 S13e 抓到）。
//
// ⇒ 断言：两个不同 upload_id 的同索引分片**必须共存**（复合索引的正确语义）。
func TestUploadChunk_TwoSessionsSameIndexCoexist(t *testing.T) {
	gormDB := newTestDB(t)
	svc := NewService(gormDB)
	ctx := context.Background()

	for _, uid := range []string{"u-a", "u-b"} {
		_, err := svc.InitiateUpload(ctx, &InitiateUploadReq{
			UploadID: uid, TotalChunks: 2, FileName: "f.bin",
		})
		require.NoError(t, err)

		// 两个会话都传 index 0
		_, err = svc.UploadChunk(ctx, &UploadChunkReq{
			UploadID:   uid,
			ChunkIndex: 0,
			ChunkHash:  "hash-" + uid,
			ChunkSize:  10,
		})
		require.NoError(t, err, "会话 %s 的 index 0 上传应成功", uid)
	}

	// 两行都在（各自的会话内 index 0）
	var rows int64
	require.NoError(t, gormDB.Model(&model.UploadChunk{}).
		Where("chunk_index = ? AND chunk_index >= 0", 0).Count(&rows).Error)
	assert.Equal(t, int64(2), rows,
		"不同上传会话的同索引分片必须共存（单列唯一索引会误杀 ⇒ CI 冒烟 S13e 抓过）")
}
