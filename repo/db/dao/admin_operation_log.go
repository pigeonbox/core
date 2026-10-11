package dao

import (
	"context"
	"time"

	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm"
)

type AdminOperationLogRepository struct {
	conn *gorm.DB // 构造注入（去全局化终态，见 doc.go）
}

// NewAdminOperationLogRepository 构造 repository；gormDB 必填（去全局化终态，无全局回退），约定见 doc.go。
func NewAdminOperationLogRepository(gormDB *gorm.DB) *AdminOperationLogRepository {
	return &AdminOperationLogRepository{conn: gormDB}
}

func (r *AdminOperationLogRepository) db() *gorm.DB {
	return r.conn
}

func (r *AdminOperationLogRepository) Create(ctx context.Context, log *model.AdminOperationLog) error {
	return r.db().WithContext(ctx).Create(log).Error
}

func (r *AdminOperationLogRepository) List(ctx context.Context, query model.AdminOperationLogQuery) ([]*model.AdminOperationLog, int64, error) {
	page, pageSize := clampPage(query.Page, query.PageSize, MaxPageSize)

	dbQuery := r.db().WithContext(ctx).Model(&model.AdminOperationLog{})

	if query.Action != "" {
		dbQuery = dbQuery.Where("action = ?", query.Action)
	}

	if query.Actor != "" {
		actorPattern, esc := LikeContains(query.Actor)
		dbQuery = dbQuery.Where("actor_name LIKE ? "+esc, actorPattern)
	}

	if query.Success != nil {
		dbQuery = dbQuery.Where("success = ?", *query.Success)
	}

	return paginate[model.AdminOperationLog](dbQuery.Order("created_at DESC"), page, pageSize)
}

// DeleteOlderThan 日志保留清理：删除 created_at 早于 cutoff 的行，返回删除行数。
func (r *AdminOperationLogRepository) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res := r.db().WithContext(ctx).Where("created_at < ?", cutoff).Delete(&model.AdminOperationLog{})
	return res.RowsAffected, res.Error
}
