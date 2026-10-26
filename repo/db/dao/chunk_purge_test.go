package dao

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm"
)

func newChunkPurgeDB(t *testing.T) *gorm.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.UploadChunk{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return g
}

// TestPurgeSoftDeletedChunks 回归（2026-10-26）：
// upload_chunks 的会话行被清理时是**软删**（软删语义被「会话复活」依赖：同 upload_id
// 重来时要能查到旧行并复活）⇒ 若不硬清理，表**只增不减**、长期运行后软删行堆积。
// ⇒ 窗口外的软删行硬清理；窗口内的保留（复活路径依赖它占着唯一键）。
func TestPurgeSoftDeletedChunks(t *testing.T) {
	g := newChunkPurgeDB(t)
	repo := NewChunkRepository(g)
	ctx := context.Background()
	const oldUID = "11111111111111111111111111111111"
	const newUID = "22222222222222222222222222222222"

	// 旧会话：控制行 + 分片行，软删时间在窗口外（40 天前）
	require.NoError(t, repo.Create(ctx, &model.UploadChunk{
		UploadID: oldUID, ChunkIndex: -1, Status: "pending",
	}))
	require.NoError(t, repo.Create(ctx, &model.UploadChunk{
		UploadID: oldUID, ChunkIndex: 0, Status: "completed", Completed: true,
	}))
	require.NoError(t, repo.DeleteByUploadID(ctx, oldUID))
	require.NoError(t, g.Unscoped().Exec(
		"UPDATE upload_chunks SET deleted_at = ? WHERE upload_id = ?",
		time.Now().Add(-40*24*time.Hour), oldUID).Error)

	// 新会话：刚软删（窗口内）
	require.NoError(t, repo.Create(ctx, &model.UploadChunk{
		UploadID: newUID, ChunkIndex: -1, Status: "pending",
	}))
	require.NoError(t, repo.DeleteByUploadID(ctx, newUID))

	purged, err := repo.PurgeSoftDeletedChunks(ctx, 30*24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, int64(2), purged, "窗口外的软删行（控制行 + 分片行）应被硬清理")

	// 窗口内的软删行仍在（复活路径依赖）
	still, err := repo.GetByUploadIDUnscoped(ctx, newUID)
	require.NoError(t, err)
	require.NotNil(t, still)

	// 窗口外的已消失
	_, err = repo.GetByUploadIDUnscoped(ctx, oldUID)
	assert.Error(t, err, "窗口外的软删行应已被硬清理")

	// 活行绝不能被误删
	require.NoError(t, repo.Create(ctx, &model.UploadChunk{
		UploadID: "33333333333333333333333333333333", ChunkIndex: -1, Status: "pending",
	}))
	_, err = repo.PurgeSoftDeletedChunks(ctx, 30*24*time.Hour)
	require.NoError(t, err)
	live, err := repo.GetByUploadID(ctx, "33333333333333333333333333333333")
	require.NoError(t, err)
	require.NotNil(t, live)
}
