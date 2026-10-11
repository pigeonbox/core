package dao

import (
	"context"
	"time"

	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm"
)

type TransferLogRepository struct {
	conn *gorm.DB // 构造注入（去全局化终态，见 doc.go）
}

// NewTransferLogRepository 构造 repository；gormDB 必填（去全局化终态，无全局回退），约定见 doc.go。
func NewTransferLogRepository(gormDB *gorm.DB) *TransferLogRepository {
	return &TransferLogRepository{conn: gormDB}
}

func (r *TransferLogRepository) db() *gorm.DB {
	return r.conn
}

func (r *TransferLogRepository) Create(ctx context.Context, log *model.TransferLog) error {
	return r.db().WithContext(ctx).Create(log).Error
}

// DayCount 按天聚合计数项（趋势统计用）
type DayCount struct {
	Date  string `json:"date"` // YYYY-MM-DD
	Count int64  `json:"count"`
}

// TrendByDay 按天统计传输次数（operation 为空 = 全部类型）。
// DATE() 在 sqlite/mysql/postgres 语义一致，跨库可用。
func (r *TransferLogRepository) TrendByDay(ctx context.Context, from time.Time, operation string) ([]DayCount, error) {
	var rows []DayCount
	q := r.db().WithContext(ctx).Model(&model.TransferLog{}).
		Select("DATE(created_at) AS date, COUNT(*) AS count").
		Where("created_at >= ?", from)
	if operation != "" {
		q = q.Where("operation = ?", operation)
	}
	err := q.Group("DATE(created_at)").Order("DATE(created_at)").Scan(&rows).Error
	return rows, err
}

func (r *TransferLogRepository) List(ctx context.Context, query model.TransferLogQuery) ([]*model.TransferLog, int64, error) {
	page, pageSize := clampPage(query.Page, query.PageSize, MaxPageSize)

	dbQuery := r.db().WithContext(ctx).Model(&model.TransferLog{})

	if query.Operation != "" {
		dbQuery = dbQuery.Where("operation = ?", query.Operation)
	}

	if query.UserID != nil {
		dbQuery = dbQuery.Where("user_id = ?", *query.UserID)
	}

	if query.Search != "" {
		like, esc := LikeContains(query.Search)
		dbQuery = dbQuery.Where(
			"file_code LIKE ? OR file_name LIKE ? OR username LIKE ? OR ip LIKE ? "+esc,
			like, like, like, like,
		)
	}

	return paginate[model.TransferLog](dbQuery.Order("created_at DESC"), page, pageSize)
}

// DeleteOlderThan 日志保留清理：删除 created_at 早于 cutoff 的行，返回删除行数。
func (r *TransferLogRepository) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res := r.db().WithContext(ctx).Where("created_at < ?", cutoff).Delete(&model.TransferLog{})
	return res.RowsAffected, res.Error
}
