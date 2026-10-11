package dao

import (
	"context"
	"time"

	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm"
)

// FileRequestRepository 寄件码 DAO（P2 反向收件）。
type FileRequestRepository struct {
	conn *gorm.DB // 构造注入（去全局化终态，见 doc.go）
}

// NewFileRequestRepository 构造 repository；gormDB 必填（去全局化终态，无全局回退），约定见 doc.go。
func NewFileRequestRepository(gormDB *gorm.DB) *FileRequestRepository {
	return &FileRequestRepository{conn: gormDB}
}

func (r *FileRequestRepository) db() *gorm.DB {
	return r.conn
}

// Create 创建投递链接
func (r *FileRequestRepository) Create(ctx context.Context, fr *model.FileRequest) error {
	return r.db().WithContext(ctx).Create(fr).Error
}

// GetByToken 按令牌取链接（软删不含）
func (r *FileRequestRepository) GetByToken(ctx context.Context, token string) (*model.FileRequest, error) {
	var fr model.FileRequest
	if err := r.db().WithContext(ctx).Where("token = ?", token).First(&fr).Error; err != nil {
		return nil, err
	}
	return &fr, nil
}

// ListByUserID 用户的投递链接（新→旧）
func (r *FileRequestRepository) ListByUserID(ctx context.Context, userID uint) ([]*model.FileRequest, error) {
	var list []*model.FileRequest
	err := r.db().WithContext(ctx).
		Where("user_id = ?", userID).
		Order("created_at DESC").
		Find(&list).Error
	return list, err
}

// Delete 软删（撤销链接；访客侧随即 404）
func (r *FileRequestRepository) Delete(ctx context.Context, userID uint, token string) (bool, error) {
	res := r.db().WithContext(ctx).
		Where("user_id = ? AND token = ?", userID, token).
		Delete(&model.FileRequest{})
	return res.RowsAffected > 0, res.Error
}

// IncrementUsage 累计投递次数与字节（并发安全）
func (r *FileRequestRepository) IncrementUsage(ctx context.Context, token string, addBytes int64) error {
	return r.db().WithContext(ctx).Model(&model.FileRequest{}).
		Where("token = ?", token).
		Updates(map[string]interface{}{
			"used_count": gorm.Expr("used_count + ?", 1),
			"recv_bytes": gorm.Expr("recv_bytes + ?", addBytes),
			"updated_at": time.Now(),
		}).Error
}
