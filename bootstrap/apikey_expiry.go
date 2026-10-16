// API Key 临期站内通知（波次3）：定期扫描 7 天内到期的有效 Key，
// 给属主发站内信（带 Webhook 外推），并打去重标记。
// 通知失败不阻塞扫描（下轮重试）；标记成功才视为已通知，绝不重复打扰。
package bootstrap

import (
	"context"
	"fmt"
	"time"

	"github.com/oekit/concurrency/async"
	"go.uber.org/zap"

	notifyApp "github.com/pigeonbox/core/app/notify"
	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/repo/db"
	"github.com/pigeonbox/core/repo/db/dao"
)

const (
	apiKeyExpiryNotifyWindow = 7 * 24 * time.Hour // 提前量
	apiKeyExpiryScanInterval = 6 * time.Hour
)

func startAPIKeyExpiryNotify() {
	repo := dao.NewUserAPIKeyRepository(db.GetDB()) // 装配层显式句柄（dao 去全局化后合法消费点）
	notifySvc := notifyApp.NewService(db.GetDB())   // 装配层显式句柄

	run := func() {
		// 单轮 panic 收编（RunSafe 同步恢复→全局 zap panic handler，同 tasks.go）：
		// 否则一轮 panic 打死 goroutine，临期通知从此静默停摆
		async.RunSafe(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			keys, err := repo.ListExpiringWithin(ctx, time.Now().Add(apiKeyExpiryNotifyWindow))
			if err != nil {
				logger.Warn("api key expiry scan failed", zap.Error(err))
				return
			}
			for _, k := range keys {
				content := fmt.Sprintf("API Key「%s」（前缀 %s…）将于 %s 过期。若仍在使用请续期（吊销后重建），不再使用建议立即吊销。",
					k.Name, k.Prefix, k.ExpiresAt.Format("2006-01-02 15:04"))
				if err := notifySvc.CreateForUserSimple(ctx, k.UserID,
					"API Key 即将过期", content, "system", "warning"); err != nil {
					logger.Warn("api key expiry notify failed",
						zap.Uint("key_id", k.ID), zap.Error(err))
					continue
				}
				if err := repo.MarkExpiryNotified(ctx, k.ID); err != nil {
					logger.Warn("api key expiry mark failed",
						zap.Uint("key_id", k.ID), zap.Error(err))
				}
			}
			if len(keys) > 0 {
				logger.Info("api key expiry notifications sent", zap.Int("count", len(keys)))
			}
		})
	}

	run() // 启动即扫一轮
	t := time.NewTicker(apiKeyExpiryScanInterval)
	for range t.C {
		run()
	}
}
