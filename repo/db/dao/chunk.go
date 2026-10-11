package dao

import (
	"context"
	"time"

	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm"
)

type ChunkRepository struct {
	conn *gorm.DB // 构造注入（去全局化终态，见 doc.go）
}

// NewChunkRepository 构造 repository；gormDB 必填（去全局化终态，无全局回退），约定见 doc.go。
func NewChunkRepository(gormDB *gorm.DB) *ChunkRepository {
	return &ChunkRepository{conn: gormDB}
}

func (r *ChunkRepository) db() *gorm.DB {
	return r.conn
}

func (r *ChunkRepository) Create(ctx context.Context, chunk *model.UploadChunk) error {
	return r.db().WithContext(ctx).Create(chunk).Error
}

func (r *ChunkRepository) GetByHash(ctx context.Context, chunkHash string, fileSize int64) (*model.UploadChunk, error) {
	var chunk model.UploadChunk
	err := r.db().WithContext(ctx).Where("chunk_hash = ? AND file_size = ? AND chunk_index = -1", chunkHash, fileSize).First(&chunk).Error
	if err != nil {
		return nil, err
	}
	return &chunk, nil
}

func (r *ChunkRepository) GetByUploadID(ctx context.Context, uploadID string) (*model.UploadChunk, error) {
	var chunk model.UploadChunk
	err := r.db().WithContext(ctx).Where("upload_id = ? AND chunk_index = -1", uploadID).First(&chunk).Error
	if err != nil {
		return nil, err
	}
	return &chunk, nil
}

func (r *ChunkRepository) GetChunkByIndex(ctx context.Context, uploadID string, chunkIndex int) (*model.UploadChunk, error) {
	var chunk model.UploadChunk
	err := r.db().WithContext(ctx).Where("upload_id = ? AND chunk_index = ? AND completed = true", uploadID, chunkIndex).First(&chunk).Error
	if err != nil {
		return nil, err
	}
	return &chunk, nil
}

func (r *ChunkRepository) UpdateUploadProgress(ctx context.Context, uploadID string, completedChunks int) error {
	return r.db().WithContext(ctx).Model(&model.UploadChunk{}).
		Where("upload_id = ? AND chunk_index = -1", uploadID).
		Updates(map[string]interface{}{
			"completed":   completedChunks,
			"retry_count": gorm.Expr("retry_count + 1"),
		}).Error
}

func (r *ChunkRepository) UpdateChunkCompleted(ctx context.Context, uploadID string, chunkIndex int, chunkHash string) error {
	// Model 必须显式指定：仅 Where+Updates(map) 时 GORM 无从解析表名，
	// 恒报 "Table not set"（回归：分片完成端点 CompleteUpload 因此恒 500）
	return r.db().WithContext(ctx).Model(&model.UploadChunk{}).
		Where("upload_id = ? AND chunk_index = ?", uploadID, chunkIndex).
		Updates(map[string]interface{}{
			"completed":  true,
			"chunk_hash": chunkHash,
			"status":     "completed",
		}).Error
}

func (r *ChunkRepository) CountCompletedChunks(ctx context.Context, uploadID string) (int64, error) {
	var count int64
	err := r.db().WithContext(ctx).Model(&model.UploadChunk{}).
		Where("upload_id = ? AND chunk_index >= 0 AND completed = true", uploadID).
		Count(&count).Error
	return count, err
}

func (r *ChunkRepository) DeleteByUploadID(ctx context.Context, uploadID string) error {
	return r.db().WithContext(ctx).Where("upload_id = ?", uploadID).Delete(&model.UploadChunk{}).Error
}

func (r *ChunkRepository) GetUploadList(ctx context.Context, page, pageSize int) ([]*model.UploadChunk, int64, error) {
	var chunks []*model.UploadChunk
	var total int64

	// 只获取控制记录（chunk_index = -1）
	query := r.db().WithContext(ctx).Model(&model.UploadChunk{}).Where("chunk_index = -1")

	// 获取总数
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	// 分页查询
	offset := (page - 1) * pageSize
	err := query.Order("created_at DESC").Offset(offset).Limit(pageSize).Find(&chunks).Error

	return chunks, total, err
}

// GetIncompleteUploads 超过 maxAge 未完成的控制记录（chunk_index=-1）。
// cutoff 由 Go 侧计算后传参——修复：此前用 SQLite 专属的
// datetime('now', '-' || ? || ' hours')，MySQL/Postgres 部署直接 SQL 报错。
func (r *ChunkRepository) GetIncompleteUploads(ctx context.Context, maxAge time.Duration) ([]*model.UploadChunk, error) {
	cutoff := time.Now().Add(-maxAge)
	var chunks []*model.UploadChunk
	err := r.db().WithContext(ctx).
		Where("chunk_index = -1 AND status != 'completed' AND updated_at < ?", cutoff).
		Find(&chunks).Error
	return chunks, err
}

// DeleteStaleSessions 删除过期会话的全部行（控制记录 + 分片行）。
// completed 为 true 时清理已完成会话的旧行（控制表防膨胀）。
func (r *ChunkRepository) DeleteStaleSessions(ctx context.Context, maxAge time.Duration, completed bool) (int64, error) {
	cutoff := time.Now().Add(-maxAge)
	q := r.db().WithContext(ctx).Where("updated_at < ?", cutoff)
	if completed {
		q = q.Where("chunk_index = -1 AND status = 'completed'")
	} else {
		q = q.Where("chunk_index = -1 AND status != 'completed'")
	}
	var controls []*model.UploadChunk
	if err := q.Find(&controls).Error; err != nil {
		return 0, err
	}
	if len(controls) == 0 {
		return 0, nil
	}
	ids := make([]string, 0, len(controls))
	for _, c := range controls {
		ids = append(ids, c.UploadID)
	}
	res := r.db().WithContext(ctx).Where("upload_id IN ?", ids).Delete(&model.UploadChunk{})
	return res.RowsAffected, res.Error
}

func (r *ChunkRepository) GetOldChunks(ctx context.Context, cutoffTime time.Time) ([]*model.UploadChunk, error) {
	var oldChunks []*model.UploadChunk
	err := r.db().WithContext(ctx).Where("created_at < ? AND chunk_index = -1", cutoffTime).Find(&oldChunks).Error
	return oldChunks, err
}

func (r *ChunkRepository) DeleteChunksByUploadIDs(ctx context.Context, uploadIDs []string) (int, error) {
	if len(uploadIDs) == 0 {
		return 0, nil
	}

	count := 0
	for _, uploadID := range uploadIDs {
		if err := r.db().WithContext(ctx).Where("upload_id = ?", uploadID).Delete(&model.UploadChunk{}).Error; err != nil {
			continue // 记录错误但继续处理其他上传
		}
		count++
	}
	return count, nil
}

func (r *ChunkRepository) GetUploadedChunkIndexes(ctx context.Context, uploadID string) ([]int, error) {
	var uploadedChunks []int
	err := r.db().WithContext(ctx).Model(&model.UploadChunk{}).
		Where("upload_id = ? AND completed = true AND chunk_index >= 0", uploadID).
		Pluck("chunk_index", &uploadedChunks).Error
	return uploadedChunks, err
}

func (r *ChunkRepository) FirstOrCreateChunk(ctx context.Context, chunk *model.UploadChunk) error {
	return r.db().WithContext(ctx).Where("upload_id = ? AND chunk_index = ?", chunk.UploadID, chunk.ChunkIndex).
		Assign(chunk).
		FirstOrCreate(chunk).Error
}

// UpdateOwner 更新会话归属字段（IP 漂移后的会话接管）
func (r *ChunkRepository) UpdateOwner(ctx context.Context, uploadID string, updates map[string]interface{}) error {
	return r.db().WithContext(ctx).Model(&model.UploadChunk{}).
		Where("upload_id = ? AND chunk_index = -1", uploadID).
		Updates(updates).Error
}

// ListSessionIDs 全量分片会话 ID（含软删，Distinct）——物理 chunks/ 目录对账用。
func (r *ChunkRepository) ListSessionIDs(ctx context.Context) ([]string, error) {
	var ids []string
	if err := r.db().WithContext(ctx).Unscoped().
		Model(&model.UploadChunk{}).Distinct().Pluck("upload_id", &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}
