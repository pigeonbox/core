package dao_preview

import (
	"context"

	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm"
)

// FilePreviewRepository 预览信息仓库
type FilePreviewRepository struct {
	conn *gorm.DB // 构造注入（去全局化终态，见 doc.go）
}

// NewFilePreviewRepository 创建预览仓库
// NewFilePreviewRepository 构造 repository；gormDB 必填（去全局化终态，无全局回退），约定见 doc.go。
func NewFilePreviewRepository(gormDB *gorm.DB) *FilePreviewRepository {
	return &FilePreviewRepository{conn: gormDB}
}

func (r *FilePreviewRepository) db() *gorm.DB {
	return r.conn
}

// Create 创建预览信息
func (r *FilePreviewRepository) Create(ctx context.Context, preview *model.FilePreview) error {
	return r.db().WithContext(ctx).Create(preview).Error
}

// GetByFileCodeID 根据文件ID获取预览信息
func (r *FilePreviewRepository) GetByFileCodeID(ctx context.Context, fileCodeID uint) (*model.FilePreview, error) {
	var preview model.FilePreview
	err := r.db().WithContext(ctx).Where("file_code_id = ?", fileCodeID).First(&preview).Error
	if err != nil {
		return nil, err
	}
	return &preview, nil
}

// Update 更新预览信息
func (r *FilePreviewRepository) Update(ctx context.Context, preview *model.FilePreview) error {
	return r.db().WithContext(ctx).Save(preview).Error
}

// Delete 删除预览信息
func (r *FilePreviewRepository) Delete(ctx context.Context, fileCodeID uint) error {
	return r.db().WithContext(ctx).Where("file_code_id = ?", fileCodeID).Delete(&model.FilePreview{}).Error
}

// ThumbnailRef 缩略图引用（file_previews 行 → 缓存文件名）。
type ThumbnailRef struct {
	FileCodeID uint
	Thumbnail  string
}

// ListThumbnailRefs 全量拉取缩略图引用（core#117：janitor 缓存对账用）。
// Thumbnail 原样返回（历史行存服务器全路径、新行存文件名，消费方按 Base 归一，
// 与 GetThumb 的取文件口径一致）。
func (r *FilePreviewRepository) ListThumbnailRefs(ctx context.Context) ([]ThumbnailRef, error) {
	var rows []model.FilePreview
	if err := r.db().WithContext(ctx).Select("file_code_id", "thumbnail").Find(&rows).Error; err != nil {
		return nil, err
	}
	refs := make([]ThumbnailRef, 0, len(rows))
	for _, row := range rows {
		refs = append(refs, ThumbnailRef{FileCodeID: row.FileCodeID, Thumbnail: row.Thumbnail})
	}
	return refs, nil
}

// DeleteOrphanRows 物理删除「主表已 purge」的预览行（file_code_id 无主）。
//
// 子行与主表同生共死（core#62 纪律）：file_codes 硬删后 file_previews 行此前
// 永久滞留（只写不删，core#117）。⚠️ 软删（回收站）的主表行**不是**孤儿——
// 恢复后预览要能接上；NOT IN 子查询直查 file_codes 表，天然含软删行，
// sqlite/mysql/pg 三家同语义（无 gorm scope 参与）。
func (r *FilePreviewRepository) DeleteOrphanRows(ctx context.Context) (int64, error) {
	res := r.db().WithContext(ctx).
		Where("file_code_id NOT IN (SELECT id FROM file_codes)").
		Delete(&model.FilePreview{})
	return res.RowsAffected, res.Error
}
