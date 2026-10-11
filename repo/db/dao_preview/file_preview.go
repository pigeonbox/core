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
