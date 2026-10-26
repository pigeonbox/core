package admin

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/model"
)

// TestCleanupStaleUploads_PurgesSoftDeletedChunkRows 回归（2026-10-26）：
// upload_chunks 的会话行被清理时是**软删**（软删语义被「会话复活」依赖）⇒ 若不硬
// 清理，表**只增不减**、长期运行后软删行堆积。
// ⇒ 断言 **janitor 真的调用了硬清理**（只测 DAO 方法会漏掉「没接线」）。
func TestCleanupStaleUploads_PurgesSoftDeletedChunkRows(t *testing.T) {
	_, svc, g := newJanitorTestEnv(t)
	j := NewJanitor(g, svc, 7)

	ctx := context.Background()
	const oldUID = "11111111111111111111111111111111"
	// 软删时间在窗口外（40 天前；janitor 的硬清理窗口是 30 天）
	require.NoError(t, g.Create(&model.UploadChunk{
		UploadID: oldUID, ChunkIndex: -1, Status: "pending",
	}).Error)
	require.NoError(t, g.Create(&model.UploadChunk{
		UploadID: oldUID, ChunkIndex: 0, Status: "completed", Completed: true,
	}).Error)
	// 软删走 DAO 方法（gorm 的 Delete 必须带 Model/Table，否则报「Table not set」）
	require.NoError(t, j.chunkRepo.DeleteByUploadID(ctx, oldUID))
	// 把软删时间回拨到窗口外（带 Model()：gorm 的 Update/Exec 不给表名会报
	// 「Table not set」）
	require.NoError(t, g.Unscoped().Model(&model.UploadChunk{}).
		Where("upload_id = ?", oldUID).
		Update("deleted_at", time.Now().Add(-40*24*time.Hour)).Error)

	_, err := j.CleanupStaleUploads(ctx, 24*time.Hour)
	require.NoError(t, err)

	var left int64
	require.NoError(t, g.Unscoped().Model(&model.UploadChunk{}).
		Where("upload_id = ?", oldUID).Count(&left).Error)
	assert.Equal(t, int64(0), left, "窗口外的软删会话行应被 janitor 硬清理（表不再只增不减）")
}
