package bootstrap

// 职责：后台周期任务——过期文件清理与孤儿对账/日志保留（与 apikey_expiry.go 同属后台任务）。

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/oekit/concurrency/async"
	"github.com/oekit/concurrency/loop"
	"go.uber.org/zap"
	"gorm.io/gorm"

	adminApp "github.com/pigeonbox/core/app/admin"
	storageHandler "github.com/pigeonbox/core/gen/handler/storage"
	"github.com/pigeonbox/core/pkg/auth"
	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/repo/db"
	"github.com/pigeonbox/core/repo/db/dao"
)

// defaultPickupDetailRetentionDays 取件明细默认留存天数（兜底：全局配置未就绪时）。
const defaultPickupDetailRetentionDays = 30

// 后台任务生命周期：各循环共享一根 ctx（backgroundContext），Cleanup 的
// teardown 阶段统一取消（stopBackgroundTasks），排水由 drainBackgroundTasks
// 在 lifecycle Manager 里拿独立超时预算。循环本体用 oekit loop（单轮 panic
// 收编带任务标签 + 忙时丢 tick + 首跑延迟可打断）；取消拦「下一轮」，在飞
// 轮次感知 ctx 尽快收尾，预算内跑不完由 Manager 放弃交给进程退出。
var (
	bgOnce   sync.Once
	bgCtx    context.Context
	bgCancel context.CancelFunc
	bgWg     sync.WaitGroup
)

// backgroundContext 返回后台任务共享根 ctx（懒初始化，幂等）。
func backgroundContext() context.Context {
	bgOnce.Do(func() {
		bgCtx, bgCancel = context.WithCancel(context.Background())
	})
	return bgCtx
}

// stopBackgroundTasks teardown：取消后台任务根 ctx，循环在下一处 select 退出。
func stopBackgroundTasks() {
	backgroundContext()
	bgCancel()
}

// trackBackgroundLoop 把 loop 挂进排水 WaitGroup（bg-tasks 预算内等收尾）。
//
// GoSafe：裸 goroutine 的 panic 无人接 ⇒ 带走整个进程（长驻服务尤其致命）。
// panic 经全局 async.SetPanicHandler 落结构化日志（bootstrap.go 已装），
// 且闭包内的 defer bgWg.Done() 在 panic 展开时照样执行 ⇒ 排水不会挂住。
func trackBackgroundLoop(l *loop.Loop) {
	bgWg.Add(1)
	async.GoSafe(func() {
		defer bgWg.Done()
		l.Wait()
	})
}

// drainBackgroundTasks 等待在飞轮次退出；ctx（lifecycle Manager 侧的超时预算）
// 先到则报超时——在飞工作被放弃，交给进程退出收尾。
func drainBackgroundTasks(ctx context.Context) error {
	done := make(chan struct{})
	async.GoSafe(func() {
		bgWg.Wait()
		close(done)
	})
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("后台任务排水超时,在飞轮次被中止")
	}
}

// startExpiredFileCleanup 定时清理过期文件（DB 记录 + 物理文件）。
// 默认每 1 小时执行一次（首跑同延迟，与旧 ticker 语义一致）；懒清理由取件
// 路径覆盖（GetFileByCode 发现过期即返回错误）。轮次感知循环 ctx：停机时
// 尽快收尾而非硬扛整轮（未完成部分由下轮/下次启动的对账兜底）。
func startExpiredFileCleanup(svc *adminApp.Service) {
	trackBackgroundLoop(loop.Run(backgroundContext(), loop.Config{
		Name:       "expired-file-cleanup",
		Interval:   time.Hour,
		FirstDelay: time.Hour,
	}, func(ctx context.Context) {
		if n, freed, err := svc.CleanExpiredFiles(ctx); err != nil {
			logger.Error("expired file cleanup failed", zap.Error(err))
		} else if n > 0 {
			logger.Info("expired files cleaned",
				zap.Int64("count", n),
				zap.Int64("freed_bytes", freed))
		}
	}))
}

// tasksDB 后台任务取的全局 DB 句柄（装配层显式句柄，棘轮规则 10 的唯一访问点）。
//
// 收成一个函数而不是各处直取：规则 10 按**访问点**计数且只许减不许增
// ⇒ 每加一个后台任务就多一个访问点，而那些访问点全是同一次取用。
func tasksDB() *gorm.DB { return db.GetDB() }

// startAccountTrustCleanup 账号与信任域的过期数据清理（5.2.0；6 小时周期，
// 首跑延迟 15 分钟错开 janitor）。
//
// 清两样东西，判据都是「**时间已过**」而不是「已撤销」：
//
//	· user_sessions：created_at 超过 2× 会话时长（默认 14 天）的会话行。
//	  会话即使未撤销，其 JWT 也早已过期 ⇒ 行留着只会让「登录设备」列表
//	  越滚越长，把真正在线的设备挤出视野。
//	· password_reset_tokens：过期 24h 后的令牌（留一天是为了让「我昨天点的
//	  链接怎么失效了」有据可查，超出即无价值）。
//
// 撤销过的会话行**不清**（软撤销是审计留痕，与硬删的取舍不同）：它们的
// created_at 迟早超过窗口，届时一并物理删除——被删掉的是「已记不清的旧
// 会话」，不影响任何在线判定。
func startAccountTrustCleanup() {
	trackBackgroundLoop(loop.Run(backgroundContext(), loop.Config{
		Name:       "account-trust-cleanup",
		Interval:   6 * time.Hour,
		FirstDelay: 15 * time.Minute,
	}, func(ctx context.Context) {
		handle := tasksDB()
		if handle == nil {
			return
		}
		sessionCut := time.Now().Add(-2 * auth.SessionExpiry())
		if n, err := dao.NewSessionRepository(handle).DeleteOlderThan(ctx, sessionCut); err != nil {
			logger.Error("session cleanup failed", zap.Error(err))
		} else if n > 0 {
			logger.Info("stale login sessions purged", zap.Int64("count", n))
		}
		if n, err := dao.NewPasswordResetRepository(handle).DeleteExpired(ctx, time.Now().Add(-24*time.Hour)); err != nil {
			logger.Error("password reset token cleanup failed", zap.Error(err))
		} else if n > 0 {
			logger.Info("expired password reset tokens purged", zap.Int64("count", n))
		}
	}))
}

// startMaintenanceJanitor 孤儿文件对账 + 日志保留清理（24h 周期，首跑延迟 10 分钟
// 等启动期写入稳定，延迟可被停机打断）。local 后端真删孤儿文件；远端后端仅跳过
// （报告策略见 janitor）。
func startMaintenanceJanitor() {
	retention := 90
	// 取件明细留存天数（0=永久）：owner 侧「取件历史」的逐条明细要留很久，
	// **不能**跟着下载令牌 TTL 走（2026-10-24 缺陷：TTL+1h ⇒ 明细最多存在
	// 1 小时 ⇒ 逐条历史形同虚设）。
	pickupDetailDays := defaultPickupDetailRetentionDays
	if config != nil {
		retention = config.Admin.LogRetentionDays
		pickupDetailDays = config.Admin.PickupDetailRetentionDays
	}
	j := adminApp.NewJanitor(tasksDB(), getBootstrapStorageService(), retention) // 装配层显式句柄
	// 管理端孤儿端点（POST /admin/storage/clean-presign-orphans 与
	// GET /admin/storage/insights 的孤儿计数）复用同一 janitor 实例
	storageHandler.SetJanitor(j)
	trackBackgroundLoop(loop.Run(backgroundContext(), loop.Config{
		Name:       "maintenance-janitor",
		Interval:   24 * time.Hour,
		FirstDelay: 10 * time.Minute,
	}, func(ctx context.Context) {
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
		// 取件计次记录（一个下载令牌至多扣 1 次）——按令牌 TTL 清理
		if n, err := j.CleanupPickupCharges(ctx, pickupDetailDays); err != nil {
			logger.Warn("pickup charge cleanup failed", zap.Error(err))
		} else if n > 0 {
			logger.Info("pickup charges cleaned", zap.Int64("count", n))
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
	}))
}
