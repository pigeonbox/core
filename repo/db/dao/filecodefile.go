package dao

import (
	"context"

	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm"
)

// FileCodeFileRepository 多文件分享子文件表 DAO（P0 多文件）。
type FileCodeFileRepository struct {
	conn *gorm.DB // 构造注入（去全局化终态，见 doc.go）
}

// NewFileCodeFileRepository 构造 repository；gormDB 必填（去全局化终态，无全局回退），约定见 doc.go。
func NewFileCodeFileRepository(gormDB *gorm.DB) *FileCodeFileRepository {
	return &FileCodeFileRepository{conn: gormDB}
}

func (r *FileCodeFileRepository) db() *gorm.DB {
	return r.conn
}

// CreateBatch 批量写入子文件行（单事务）。
func (r *FileCodeFileRepository) CreateBatch(ctx context.Context, files []*model.FileCodeFile) error {
	if len(files) == 0 {
		return nil
	}
	return r.db().WithContext(ctx).Create(&files).Error
}

// ListByFileCodeID 按 FileCodeID 列出子文件（按 SortOrder, ID 稳定排序）。
// 软删除的行自动排除。
func (r *FileCodeFileRepository) ListByFileCodeID(ctx context.Context, fileCodeID uint) ([]*model.FileCodeFile, error) {
	var files []*model.FileCodeFile
	err := r.db().WithContext(ctx).
		Where("file_code_id = ?", fileCodeID).
		Order("sort_order ASC, id ASC").
		Find(&files).Error
	return files, err
}

// GetByID 取单个子文件行。
func (r *FileCodeFileRepository) GetByID(ctx context.Context, id uint) (*model.FileCodeFile, error) {
	var f model.FileCodeFile
	if err := r.db().WithContext(ctx).First(&f, id).Error; err != nil {
		return nil, err
	}
	return &f, nil
}

// CountByFileCodeID 子文件行数（软删除不含）。
func (r *FileCodeFileRepository) CountByFileCodeID(ctx context.Context, fileCodeID uint) (int64, error) {
	var n int64
	err := r.db().WithContext(ctx).Model(&model.FileCodeFile{}).
		Where("file_code_id = ?", fileCodeID).Count(&n).Error
	return n, err
}

// SoftDeleteByFileCodeIDs 按 FileCodeID 批量软删子文件行。
// 物理文件删除由调用方先行完成（DB 软删可回滚，物理删不可）。
func (r *FileCodeFileRepository) SoftDeleteByFileCodeIDs(ctx context.Context, fileCodeIDs []uint) error {
	if len(fileCodeIDs) == 0 {
		return nil
	}
	return r.db().WithContext(ctx).
		Where("file_code_id IN ?", fileCodeIDs).
		Delete(&model.FileCodeFile{}).Error
}

// CountByFileCodeIDs 批量统计多个分享的子文件数（列表页一次 GROUP BY，避免 N+1）。
// 返回 file_code_id → count；无子行的分享不出现在结果中（调用方按 0 处理）。
func (r *FileCodeFileRepository) CountByFileCodeIDs(ctx context.Context, fileCodeIDs []uint) (map[uint]int64, error) {
	out := make(map[uint]int64, len(fileCodeIDs))
	if len(fileCodeIDs) == 0 {
		return out, nil
	}
	var rows []struct {
		FileCodeID uint  `gorm:"column:file_code_id"`
		Cnt        int64 `gorm:"column:cnt"`
	}
	err := r.db().WithContext(ctx).Model(&model.FileCodeFile{}).
		Select("file_code_id, COUNT(*) AS cnt").
		Where("file_code_id IN ?", fileCodeIDs).
		Group("file_code_id").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.FileCodeID] = row.Cnt
	}
	return out, nil
}
