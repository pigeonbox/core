package bootstrap

// 分享即将过期提醒（5.5.0）。
//
// # 为什么需要它
//
// 分享是**有期限的**（默认 7 天），但寄件人往往把它当成「发出去就一直在」——
// 到期后取件码失效、文件被清理，取件人才发现「码不对」。通知渠道（站内信 /
// webhook / SMTP / Telegram / Bark）从 4.9.x 起已经齐了，缺的只是**在正确的
// 时刻产生这条消息**。本文件只做这件事，不新增任何渠道。
//
// # 三个不变性（改代码前先读）
//
//  1. **一个分享只提醒一次**：判据是 `file_codes.expiry_reminded_at` 列。
//     抢标记（条件 UPDATE）成功才发 ⇒ 多副本部署下也只有一个副本会发。
//  2. **抢了就得发出去，发不出去要还回去**：发送失败立刻 ClearExpiryReminded，
//     下一轮重试。宁可晚一轮，不可静默丢——这与「API Key 到期提醒」先发后标的
//     顺序不同，原因见下一段。
//  3. **短命分享不提醒**：寿命（expired_at - created_at）本身就短于提前量的分享
//     不进入提醒范围。刚发出去 1 小时的分享对它说「还有 24 小时过期」是噪音，
//     而这类分享在窗口判据下**恰好会被立刻命中**（所以必须显式排除）。
//
// # 为什么是「抢标记 → 发 → 失败回滚」而不是「发 → 标记」
//
// app/key 的到期提醒走后者（先发后标），因为它只有一份、且单副本居多。本任务
// 要面对 public×N 多副本（见 deployment.mode）：先发后标会让 N 个副本各发一次。
// 抢标记把发送权**仲裁成一次**，代价是「抢到但没发出去」的时间窗（用回滚覆盖）。

import (
	"context"
	"fmt"
	"time"

	"github.com/oekit/concurrency/loop"
	"go.uber.org/zap"

	notifyApp "github.com/pigeonbox/core/app/notify"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/repo/db"
	"github.com/pigeonbox/core/repo/db/dao"
)

const (
	// expiryReminderTick 轮询节拍（**不是**提醒频率）。
	//
	// 为什么节拍与频率分开：频率（notify.expiry_reminder.interval_minutes）是管理台
	// 可改的配置，而 loop 的 Interval 在注册时固定 ⇒ 把 60 分钟写进 loop 就等于
	// 「改配置要重启」，而管理台其它开关都是即时生效的。折中：每分钟醒一次，
	// 用「距上轮是否已过 interval」决定要不要真扫；空闲时只是一次时间比较。
	expiryReminderTick = time.Minute

	expiryReminderDefaultAdvanceHours    = 24
	expiryReminderMinAdvanceHours        = 1
	expiryReminderMaxAdvanceHours        = 720 // 30 天：超过「分享有效期本身」就没意义了
	expiryReminderDefaultIntervalMinutes = 60
	expiryReminderMinIntervalMinutes     = 1
	expiryReminderMaxIntervalMinutes     = 1440
)

// expiryReminderNotifier 本任务真正用到的那一个方法（测试可替身）。
//
// 不直接写 *notifyApp.Service：这样单测能注入「必定失败」的发送器来验证回滚，
// 而不必去 mock 整个通知域。
type expiryReminderNotifier interface {
	CreateForUserSimple(ctx context.Context, userID uint, title, content, notifyType, level string) error
}

// clampExpiryReminderConfig 把配置收敛到有效区间（越界按边界，0 取默认）。
//
// 为什么收敛而不是报错：这是运维随手填的洞察类参数，填 0 或 99999 的意图都是
// 「让它跑起来」，直接拒掉会让「提醒不生效」变成一个没有出口的哑谜。
func clampExpiryReminderConfig(cfg conf.ExpiryReminderConfig) (advance time.Duration, interval time.Duration) {
	advH := cfg.AdvanceHours
	switch {
	case advH == 0:
		advH = expiryReminderDefaultAdvanceHours
	case advH < expiryReminderMinAdvanceHours:
		advH = expiryReminderMinAdvanceHours
	case advH > expiryReminderMaxAdvanceHours:
		advH = expiryReminderMaxAdvanceHours
	}
	itvM := cfg.IntervalMinutes
	switch {
	case itvM == 0:
		itvM = expiryReminderDefaultIntervalMinutes
	case itvM < expiryReminderMinIntervalMinutes:
		itvM = expiryReminderMinIntervalMinutes
	case itvM > expiryReminderMaxIntervalMinutes:
		itvM = expiryReminderMaxIntervalMinutes
	}
	return time.Duration(advH) * time.Hour, time.Duration(itvM) * time.Minute
}

// runExpiryReminderRound 跑一轮提醒，返回**实际发出**的条数。
//
// 参数全显式（repo / notifier / cfg / now）而不取全局：这是单测唯一入口，
// 也是「同一个函数在装配层与测试里行为一致」的保证。
func runExpiryReminderRound(
	ctx context.Context,
	repo *dao.FileCodeRepository,
	notifier expiryReminderNotifier,
	cfg conf.ExpiryReminderConfig,
	now time.Time,
) (int, error) {
	if !cfg.Enabled {
		return 0, nil
	}
	if repo == nil || notifier == nil {
		return 0, nil
	}
	advance, _ := clampExpiryReminderConfig(cfg)

	items, err := repo.ListExpiringForReminder(ctx, now, advance, dao.ExpiryReminderCandidateLimit)
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, f := range items {
		if f == nil || f.UserID == nil || f.ExpiredAt == nil {
			continue
		}
		// 短命分享不提醒（见文件头不变性 3）。created_at 为零值说明来源异常，按短命处理。
		if f.CreatedAt.IsZero() || f.ExpiredAt.Sub(f.CreatedAt) <= advance {
			continue
		}
		// 抢标记：抢不到说明另有副本（或上一轮）已经在处理这条。
		won, err := repo.MarkExpiryReminded(ctx, f.ID, now)
		if err != nil {
			logger.Warn("expiry reminder mark failed", zap.Uint("file_code_id", f.ID), zap.Error(err))
			continue
		}
		if !won {
			continue
		}
		remaining := f.ExpiredAt.Sub(now)
		hours := int(remaining.Round(time.Hour).Hours())
		if hours < 1 {
			hours = 1
		}
		content := fmt.Sprintf(
			"你发布的分享「%s」将于 %s 过期（约剩 %d 小时）。到期后取件码失效、文件会被清理；如需保留，请到「我的分享」延长过期时间。",
			f.Code, f.ExpiredAt.Local().Format("2006-01-02 15:04"), hours)
		if err := notifier.CreateForUserSimple(ctx, *f.UserID, "分享即将过期", content, notifyTypeShareExpiry, "warning"); err != nil {
			// 抢到了却没发出去 ⇒ 立刻还回去，下一轮重试（不变性 2）。
			if clearErr := repo.ClearExpiryReminded(ctx, f.ID); clearErr != nil {
				logger.Error("expiry reminder rollback failed",
					zap.Uint("file_code_id", f.ID), zap.Error(clearErr))
			}
			logger.Warn("expiry reminder notify failed", zap.Uint("file_code_id", f.ID), zap.Error(err))
			continue
		}
		sent++
	}
	return sent, nil
}

// startExpiryReminder 注册提醒循环（配置读不到或未启用时静默空转，不报错）。
func startExpiryReminder() {
	// 装配层显式句柄（dao 去全局化后合法消费点）：循环在进程生命周期内只注册一次，
	// 句柄由装配层取一次并闭包持有，轮次内不再回读全局单例。
	handle := db.GetDB()
	if handle == nil {
		return
	}
	repo := dao.NewFileCodeRepository(handle)
	notifier := notifyApp.NewService(handle)

	var lastRun time.Time
	trackBackgroundLoop(loop.Run(backgroundContext(), loop.Config{
		Name:     "share-expiry-reminder",
		Interval: expiryReminderTick,
		// 首跑立即：重启用不必先等一个节拍（interval 判据仍在，不会补发历史）。
	}, func(ctx context.Context) {
		// 全局配置直读（规则 8 已登记）：提醒开关与两个数字是**管理台热改**的配置，
		// 而循环在启动时注册、无法重启换参数 ⇒ 每轮都必须读当前生效快照。
		// 这条直读本身就是「配置改了要立刻生效」的实现，不能改成构造注入。
		cfg := conf.GetGlobalConfig()
		if cfg == nil {
			return
		}
		_, interval := clampExpiryReminderConfig(cfg.Notify.ExpiryReminder)
		now := time.Now()
		// 节拍判定用 wall clock 差（不是 tick 计数）：进程休眠 / 长时间 GC 后
		// 计数会漂，而「距上一轮过了多久」不会。
		if !lastRun.IsZero() && now.Sub(lastRun) < interval {
			return
		}
		lastRun = now
		sent, err := runExpiryReminderRound(ctx, repo, notifier, cfg.Notify.ExpiryReminder, now)
		if err != nil {
			logger.Warn("share expiry reminder round failed", zap.Error(err))
			return
		}
		if sent > 0 {
			logger.Info("share expiry reminders sent", zap.Int("count", sent))
		}
	}))
}

// 编译期断言：notify 域的服务确实满足本任务的窄接口（改了签名立刻红）。
var _ expiryReminderNotifier = (*notifyApp.Service)(nil)

// 提醒消息的类型标签（前端按它分组；写成常量防手写漂移）。
const notifyTypeShareExpiry = "share_expiry"
