package bootstrap

// 职责：后台周期任务——过期文件清理与孤儿对账/日志保留（与 apikey_expiry.go 同属后台任务）。

import (
	"context"
	"time"

	"go.uber.org/zap"

	adminApp "github.com/pigeonbox/core/app/admin"
	storageHandler "github.com/pigeonbox/core/gen/handler/storage"
	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/repo/db"
)

// startExpiredFileCleanup 定时清理过期文件（DB 记录 + 物理文件）。
// 默认每 1 小时执行一次；懒清理由取件路径覆盖（GetFileByCode 发现过期即返回错误）。
func startExpiredFileCleanup(svc *adminApp.Service) {
	interval := time.Hour
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	ctx := context.Background()
	for range ticker.C {
		if n, freed, err := svc.CleanExpiredFiles(ctx); err != nil {
			logger.Error("expired file cleanup failed", zap.Error(err))
		} else if n > 0 {
			logger.Info("expired files cleaned",
				zap.Int64("count", n),
				zap.Int64("freed_bytes", freed))
		}
	}
}

// startMaintenanceJanitor 孤儿文件对账 + 日志保留清理（24h 周期，首跑延迟 10 分钟
// 等启动期写入稳定）。local 后端真删孤儿文件；远端后端仅跳过（报告策略见 janitor）。
func startMaintenanceJanitor() {
	retention := 90
	if config != nil {
		retention = config.Admin.LogRetentionDays
	}
	j := adminApp.NewJanitor(db.GetDB(), getBootstrapStorageService(), retention) // 装配层显式句柄
	// 管理端孤儿端点（POST /admin/storage/clean-presign-orphans 与
	// GET /admin/storage/insights 的孤儿计数）复用同一 janitor 实例
	storageHandler.SetJanitor(j)
	time.Sleep(10 * time.Minute)
	ctx := context.Background()
	run := func() {
		if _, removed, err := j.ReconcileOrphans(ctx); err != nil {
			logger.Warn("orphan reconcile failed", zap.Error(err))
		} else if removed > 0 {
			logger.Info("orphan files removed", zap.Int("count", removed))
		}
		if n, err := j.CleanupLogs(ctx); err != nil {
			logger.Warn("log retention cleanup failed", zap.Error(err))
		} else if n > 0 {
			logger.Info("retention logs cleaned", zap.Int64("count", n))
		}
		if dirs, err := j.CleanupStaleUploads(ctx, 24*time.Hour); err != nil {
			logger.Warn("stale chunk session cleanup failed", zap.Error(err))
		} else if dirs > 0 {
			logger.Info("stale chunk dirs cleaned", zap.Int("count", dirs))
		}
		// 远端 presign 孤儿清理（Init 后未 Complete 的直传残留；
		// 本地后端自动跳过，由 ReconcileOrphans 覆盖）
		if n, err := j.CleanRemotePresignOrphans(ctx, 24*time.Hour); err != nil {
			logger.Warn("remote presign orphan cleanup failed", zap.Error(err))
		} else if n > 0 {
			logger.Info("remote presign orphan objects removed", zap.Int("count", n))
		}
	}
	run()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		run()
	}
}
