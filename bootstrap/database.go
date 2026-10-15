package bootstrap

// 职责：数据库初始化与默认管理员——InitDatabase、CreateDefaultAdmin、
// restoreRuntimeStorage（DB 持久化存储配置恢复）。

import (
	"context"
	"fmt"
	"log"
	"os"

	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	configApp "github.com/pigeonbox/core/app/config"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/repo/db"
	"github.com/pigeonbox/core/repo/db/model"
)

// InitDatabase 初始化数据库。
//
// 迁移策略（按配置）：
//   - database.migrate=true：先执行版本化迁移(migrations/*.sql)，适合生产/需要版本控制
//   - database.auto_migrate（默认 true）：GORM AutoMigrate，开发友好、自动补表/列
//   - 两者可共存：版本化迁移建表后，AutoMigrate 兜底补充新字段
func InitDatabase(config *conf.DatabaseConfig) (*gorm.DB, error) {
	// 创建数据目录
	if config.Driver == "sqlite" {
		dbPath := config.DBName
		if dbPath != ":memory:" {
			log.Printf("SQLite database path: %s", dbPath)
		}
	}

	// 初始化数据库连接（db.Init 内部会执行 AutoMigrate 兜底）
	err := db.Init(config)
	if err != nil {
		return nil, fmt.Errorf("failed to connect database: %w", err)
	}

	database := db.GetDB()

	// 版本化迁移（企业级，可选）
	if config.Migrate {
		log.Println("Running versioned database migrations...")
		migrator, err := db.NewMigrator(database, config.Driver)
		if err != nil {
			return nil, fmt.Errorf("failed to create migrator: %w", err)
		}
		applied, err := migrator.Up()
		if err != nil {
			return nil, fmt.Errorf("versioned migration failed: %w", err)
		}
		if len(applied) > 0 {
			log.Printf("Applied %d migration(s): %v", len(applied), applied)
		} else {
			log.Println("No new migrations to apply (already up to date)")
		}
		// AutoMigrate 兜底：补充 baseline 未覆盖的表（如 file_previews/notifies）
		if err := database.AutoMigrate(
			&model.FilePreview{},
			&model.Notify{},
		); err != nil {
			logger.Error("AutoMigrate fallback for previews failed", zap.Error(err))
		}
	}

	log.Println("Database initialized successfully")
	return database, nil
}

// CreateDefaultAdmin 创建默认管理员。
//
// 密码来源（优先级）：PB_ADMIN_PASSWORD 环境变量 > 默认 admin123。
// 密码用 bcrypt 现场哈希（此前硬编码的哈希与 admin123 不匹配，导致管理员无法登录）。
// 生产模式（app.production 或 server.mode=release）下若未注入 PB_ADMIN_PASSWORD
// 且库中无管理员，返回 ErrInsecureDefaultAdmin 由调用方终止启动——已知弱口令
// 不允许上线（2026-10-08 审计：此前仅 Warn，与 JWT secret 的全环境 fail-fast 不对称）。
// 仅在"即将创建"时校验：库中已有 admin 的存量部署升级不受影响（每次启动都查会
// 打断未配 env 的老部署）。cfg 允许 nil（视为非生产，保持旧行为）。
func CreateDefaultAdmin(database *gorm.DB, cfg *conf.AppConfiguration) error {
	var count int64
	database.Model(&model.User{}).Where("role = ?", "admin").Count(&count)

	if count > 0 {
		log.Println("Admin user already exists")
		return nil
	}

	// 密码：env 注入优先，否则默认 admin123
	password := os.Getenv("PB_ADMIN_PASSWORD")
	if password == "" {
		if cfg != nil && cfg.IsProduction() {
			return fmt.Errorf("%w: set PB_ADMIN_PASSWORD env to a strong password, or set PB_DISABLE_DEFAULT_ADMIN=true to configure via the /setup wizard (known credentials admin/admin123 must not ship in production)", ErrInsecureDefaultAdmin)
		}
		password = "admin123"
	}

	// 现场生成 bcrypt 哈希（避免硬编码哈希与明文不一致）
	hashed, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("failed to hash admin password: %w", err)
	}

	admin := &model.User{
		Username:     "admin",
		Email:        "admin@pigeonbox.local",
		PasswordHash: string(hashed),
		Nickname:     "Administrator",
		Role:         "admin",
		Status:       "active",
	}

	if err := database.Create(admin).Error; err != nil {
		return fmt.Errorf("failed to create admin user: %w", err)
	}

	if os.Getenv("PB_ADMIN_PASSWORD") == "" {
		logger.Warn("Default admin created with default password 'admin123' — change it immediately in production (set PB_ADMIN_PASSWORD for a custom one)")
	} else {
		logger.Info("Default admin created with password from PB_ADMIN_PASSWORD")
	}
	return nil
}

// restoreRuntimeStorage 启动时把 system_configs.runtime_storage 恢复进全局配置。
// 优先级：env（PB_STORAGE_TYPE/PB_STORAGE_PATH）> DB（管理端在线修改的意图，
// 晚于 yaml）> yaml。DB 无记录时不动 conf（yaml/env 生效）。
// 2026-10-15 起 env>DB 已推广为全部托管段统一规则（见 env_reapply.go）。
func restoreRuntimeStorage() {
	rs := configApp.Default().LoadRuntimeStorage(context.Background()) // DB 已于 InitDatabase 就绪
	if rs == nil {
		return
	}
	if rs.Type != "" {
		config.Storage.Type = rs.Type
	}
	if rs.StoragePath != "" {
		config.Storage.StoragePath = rs.StoragePath
	}
	if rs.S3 != nil {
		config.Storage.S3 = rs.S3
	}
	if rs.WebDAV != nil {
		config.Storage.WebDAV = rs.WebDAV
	}
	// 全段恢复（历史代码只回填 Type/StoragePath/S3/WebDAV，新类型段丢失会导致
	// 重启后构建失败 → 静默降级 local，与管理端已切换的状态不一致）
	if rs.FTP != nil {
		config.Storage.FTP = rs.FTP
	}
	if rs.SFTP != nil {
		config.Storage.SFTP = rs.SFTP
	}
	if rs.AzureBlob != nil {
		config.Storage.AzureBlob = rs.AzureBlob
	}
	if rs.HDFS != nil {
		config.Storage.HDFS = rs.HDFS
	}
	if rs.OneDrive != nil {
		config.Storage.OneDrive = rs.OneDrive
	}
	if rs.OSS != nil {
		config.Storage.OSS = rs.OSS
	}
	if rs.COS != nil {
		config.Storage.COS = rs.COS
	}
	if rs.BOS != nil {
		config.Storage.BOS = rs.BOS
	}
	if rs.KS3 != nil {
		config.Storage.KS3 = rs.KS3
	}
	if rs.OBS != nil {
		config.Storage.OBS = rs.OBS
	}
	// env 优先级最高：显式注入的环境变量覆盖 DB 恢复值
	if v := os.Getenv("PB_STORAGE_TYPE"); v != "" {
		config.Storage.Type = v
	}
	if v := os.Getenv("PB_STORAGE_PATH"); v != "" {
		config.Storage.StoragePath = v
	}
	log.Println("runtime storage config restored from database, type =", config.Storage.Type)
}
