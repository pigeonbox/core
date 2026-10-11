package dao

import (
	"context"
	"errors"
	"fmt"

	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm"
)

// ErrDBNotInitialized 全局 DB 未初始化（如纯内存单测、bootstrap 早期阶段）。
var ErrDBNotInitialized = errors.New("dao: database not initialized")

// SystemConfigRepository 系统配置持久化（单行，id=1）。
type SystemConfigRepository struct {
	conn *gorm.DB // 构造注入（去全局化终态，见 doc.go）
}

// NewSystemConfigRepository 构造 repository；gormDB 必填（去全局化终态，无全局回退），约定见 doc.go。
func NewSystemConfigRepository(gormDB *gorm.DB) *SystemConfigRepository {
	return &SystemConfigRepository{conn: gormDB}
}

func (r *SystemConfigRepository) db() *gorm.DB {
	return r.conn
}

// Optimize 数据库优化（sqlite: VACUUM 回收空间+ANALYZE 刷新统计；
// mysql: ANALYZE 核心表；postgres: VACUUM ANALYZE；其他后端 no-op）。
// 返回 (驱动名, 执行说明)。VACUUM 不能在事务内执行，gorm Exec 默认非事务可直接跑。
func (r *SystemConfigRepository) Optimize(ctx context.Context) (string, string, error) {
	g := r.db()
	if g == nil {
		return "", "", ErrDBNotInitialized
	}
	dialector := g.Dialector
	dialect := dialector.Name()
	switch dialect {
	case "sqlite":
		if err := g.Exec("VACUUM").Error; err != nil {
			return dialect, "", fmt.Errorf("VACUUM 失败: %w", err)
		}
		if err := g.Exec("ANALYZE").Error; err != nil {
			return dialect, "", fmt.Errorf("ANALYZE 失败: %w", err)
		}
		return dialect, "VACUUM 回收空间 + ANALYZE 刷新统计已完成", nil
	case "mysql":
		if err := g.Exec("ANALYZE TABLE file_codes, users, transfer_logs, admin_operation_logs, file_code_files").Error; err != nil {
			return dialect, "", fmt.Errorf("ANALYZE TABLE 失败: %w", err)
		}
		return dialect, "核心表统计信息已刷新（空间回收请由 DBA 择期执行 OPTIMIZE TABLE）", nil
	case "postgres":
		if err := g.Exec("VACUUM ANALYZE").Error; err != nil {
			return dialect, "", fmt.Errorf("VACUUM ANALYZE 失败: %w", err)
		}
		return dialect, "VACUUM ANALYZE 已完成", nil
	default:
		return dialect, "当前数据库后端无需在线优化", nil
	}
}

// Get 读取配置记录；无记录时返回 (nil, nil)。
func (r *SystemConfigRepository) Get(ctx context.Context) (*model.SystemConfigRecord, error) {
	g := r.db()
	if g == nil {
		return nil, ErrDBNotInitialized
	}
	var rec model.SystemConfigRecord
	err := g.WithContext(ctx).First(&rec).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &rec, nil
}

// Save 保存配置：已存在则更新首行，否则创建（保证全表至多一行）。
func (r *SystemConfigRepository) Save(ctx context.Context, rec *model.SystemConfigRecord) error {
	g := r.db()
	if g == nil {
		return ErrDBNotInitialized
	}
	existing, err := r.Get(ctx)
	if err != nil {
		return err
	}
	if existing == nil {
		return g.WithContext(ctx).Create(rec).Error
	}
	rec.ID = existing.ID
	rec.CreatedAt = existing.CreatedAt
	return g.WithContext(ctx).Model(existing).UpdateColumn("data", rec.Data).Error
}
