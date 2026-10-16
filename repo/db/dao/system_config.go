package dao

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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

// errRetryableConflict 可写冲突哨兵（不外泄）：唯一键撞键（MySQL gap-lock
// 并发首写窗口）或 SQLite 单写者快照过期（BUSY_SNAPSHOT/locked——事务内先读
// 后写，读快照建立后他方已提交）。不在事务内直接转更新——已建立的
// 一致性快照看不到并发胜出行（MySQL RR / SQLite WAL 皆然），
// 必须新事务新快照才可达更新路径。
var errRetryableConflict = errors.New("dao: system_config write conflict, retry once")

// Save 保存配置：已存在则更新首行，否则创建（保证全表至多一行）。
// 事务+行锁+撞键重试语义参考 oekit/pkg gormx（MIT）：
//   - 事务内锁读防并发双 Create；glebarez/sqlite 不支持 FOR UPDATE，跳行锁
//     （sqlite 单写者语义由事务本身保证，照 oekit gormrepo 先例）
//   - Create 撞键/单写者 busy（并发首写窗口）→ 整体重试一次，新事务
//     锁读必见胜出行，转更新路径收敛（重试仅一次，防活锁）
func (r *SystemConfigRepository) Save(ctx context.Context, rec *model.SystemConfigRecord) error {
	g := r.db()
	if g == nil {
		return ErrDBNotInitialized
	}
	for attempt := 0; ; attempt++ {
		err := g.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			q := tx
			// 断链赋值规避 QF1008（gorm 嵌入提升字段链式选择器两头触发，
			// 先例见 user_shares 类型纠偏专项）
			dial := tx.Dialector
			if dial.Name() != "sqlite" {
				q = q.Clauses(clause.Locking{Strength: "UPDATE"})
			}
			var existing model.SystemConfigRecord
			rerr := q.First(&existing).Error
			if rerr != nil && !errors.Is(rerr, gorm.ErrRecordNotFound) {
				return rerr
			}
			if rerr == nil {
				rec.ID, rec.CreatedAt = existing.ID, existing.CreatedAt
				return tx.Model(&existing).UpdateColumn("data", rec.Data).Error
			}
			if cerr := tx.Create(rec).Error; cerr != nil {
				if isUniqueViolation(cerr) || isSQLiteBusy(cerr) {
					return errRetryableConflict
				}
				return cerr
			}
			return nil
		})
		if attempt == 0 && errors.Is(err, errRetryableConflict) {
			continue // 重试一次：新事务新快照，锁读必见并发胜出行
		}
		return err
	}
}

// isUniqueViolation 撞唯一键判据：gorm.ErrDuplicatedKey 映射（需 gorm
// TranslateError，core 未开，见 repo/db/database.go——mysql/pg 上该分支恒不
// 命中）+ 三家驱动裸 SQL 文案子串，照 oekit gorm@v0.2.0/gormx/transaction.go
// 先例并补 sqlite 分支：
//   - sqlite  "UNIQUE constraint failed: …"
//   - mysql   "Error 1062: Duplicate entry …"（大写 D）
//   - pg      "… duplicate key value violates unique constraint …"（23505，小写）
//
// 按各家实际文案精确匹配，保持窄判据不加裸 "constraint failed"。
func isUniqueViolation(err error) bool {
	msg := err.Error()
	return errors.Is(err, gorm.ErrDuplicatedKey) ||
		strings.Contains(msg, "UNIQUE constraint failed") || // sqlite
		strings.Contains(msg, "Duplicate entry") || // mysql 1062
		strings.Contains(msg, "duplicate key value") // pg 23505
}

// isSQLiteBusy SQLite 单写者 busy（含读快照过期后的 BUSY_SNAPSHOT 升级失败；
// 驱动报错文案不一，按子串匹配）。
func isSQLiteBusy(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "SQLITE_BUSY")
}
