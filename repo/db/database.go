package db

import (
	"fmt"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/repo/db/model"
	"go.uber.org/zap"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// DB 全局数据库实例——**装配层显式句柄**（dao 去全局化终态）：
// dao 与 app/ 一律构造注入 *gorm.DB，不读全局；本句柄仅供 bootstrap 初始化
// 结果出口（identity loader/定时任务/库式下游 fnos·qnap SSO 显式取用）。
var DB *gorm.DB

func Init(cfg *conf.DatabaseConfig) error {
	var err error
	var dialector gorm.Dialector

	gormConfig := &gorm.Config{
		Logger: logger.Default.LogMode(logger.Info),
	}

	switch cfg.Driver {
	case "mysql":
		dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=True&loc=Local",
			cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.DBName)
		dialector = mysql.Open(dsn)
	case "postgres":
		dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
			cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.DBName)
		dialector = postgres.Open(dsn)
	case "sqlite":
		dialector = sqlite.Open(cfg.DBName)
	default:
		return fmt.Errorf("unsupported database driver: %s", cfg.Driver)
	}

	DB, err = gorm.Open(dialector, gormConfig)
	if err != nil {
		return fmt.Errorf("failed to connect database: %w", err)
	}

	if cfg.Driver != "sqlite" {
		sqlDB, err := DB.DB()
		if err != nil {
			return fmt.Errorf("failed to get sql.DB: %w", err)
		}

		sqlDB.SetMaxIdleConns(10)
		sqlDB.SetMaxOpenConns(100)
		sqlDB.SetConnMaxLifetime(time.Hour)
	}

	// 自动迁移数据库表（开发默认；若启用版本化迁移则跳过，避免与 baseline 冲突）
	// AutoMigrate 字段零值视为 true（兼容旧配置），显式 false 时跳过
	if cfg.Migrate {
		// 版本化迁移由 bootstrap 接管，这里跳过 AutoMigrate 避免表已存在冲突
		zap.L().Info("Database connected (auto_migrate skipped, versioned migration will run)", zap.String("driver", cfg.Driver))
		return nil
	}
	if err := autoMigrate(); err != nil {
		return fmt.Errorf("failed to auto migrate: %w", err)
	}

	zap.L().Info("Database connected successfully", zap.String("driver", cfg.Driver))
	return nil
}

func Close() error {
	if DB != nil {
		sqlDB, err := DB.DB()
		if err != nil {
			return err
		}
		return sqlDB.Close()
	}
	return nil
}

// dbMu 保护全局 DB 指针的读写（生产启动写一次；测试逐用例替换 + 异步
// goroutine（取件通知等 fire-and-forget）并发读，-race 下曾报数据竞争）
var dbMu sync.RWMutex

func GetDB() *gorm.DB {
	dbMu.RLock()
	defer dbMu.RUnlock()
	return DB
}

func SetDatabaseInstance(db *gorm.DB) {
	dbMu.Lock()
	DB = db
	dbMu.Unlock()
}

// autoMigrate 自动迁移数据库表
//
// ⚠️ 清单**不在这里手写** ⇒ 统一取自 `model.AutoMigrateModels()`
// （`repo/db/model/registry.go` = 全部领域模型的**单一真相源**）。
//
// 为什么挪走：此前这里是一份**手写清单**，与模型定义文件**两处漂移**，
// 漂移的后果是**静默**的——漏登记 ⇒ 升级路径的老库上那张表永远缺失
// （v0.11.1 实录：漏登 FilePreview ⇒ 所有升级库预览域整体 404）。
// 而「换库」（SQLite→PG/MySQL）要**逐表**搬数据 ⇒ 漏一张表 = 那张表的数据
// 在换库后**静默丢失** ⇒ 两处漂移的风险从「功能缺失」升级为「数据丢失」。
//
// ⇒ 清单收敛到一处，且由 `model/registry_guard_test.go` 的 AST 守卫
// **强制**它与模型定义一致（新增模型没登记 ⇒ 提交即红）。
func autoMigrate() error {
	return DB.AutoMigrate(model.AutoMigrateModels()...)
}
