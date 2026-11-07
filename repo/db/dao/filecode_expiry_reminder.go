package dao

// filecode_expiry_reminder.go — 「分享即将过期」提醒的查询与去重标记（5.5.0）。
//
// 判据与去重语义集中在这一层：提醒任务（bootstrap）只负责「按顺序做三件事」——
// 取候选 → 抢标记 → 发消息，失败回滚标记。这样多副本部署时不会 N 倍重复打扰。

import (
	"context"
	"time"

	"github.com/pigeonbox/core/repo/db/model"
)

// ExpiryReminderCandidateLimit 单轮最多处理的分享条数。
//
// 为什么要上限：一次提醒会同时走站内信 + 已配置的外部渠道（webhook/SMTP/
// Telegram/Bark），一轮打进上千条既会撞外部渠道的速率限制，也会把单轮拖长
// （停机排水预算有限）。超出的部分下一轮继续——标记是逐条写，所以不会重复。
const ExpiryReminderCandidateLimit = 200

// ListExpiringForReminder 列出「即将过期且尚未提醒过」的**用户分享**。
//
// 判据（缺一不可，任何一条放宽都会产生明确的不良后果）：
//
//   - user_id IS NOT NULL —— 匿名分享没有收件人，提醒无处可送；
//   - status = normal —— blocked / pending_review 本来就不能取件，提醒会误导；
//   - expired_count <> 0 —— 剩余次数已耗尽（0）时「还能取」是假的（-1 = 无限，>0 = 还有）；
//   - expired_at ∈ (now, now+advance] —— 已过期的不归这里管（那是过期清理任务）；
//   - expiry_reminded_at IS NULL —— 一个分享只提醒一次。
//
// ⚠️ 「寿命长于提前量」这条**不在 SQL 里表达**：`expired_at - created_at` 在
// SQLite/MySQL/Postgres 三种写法互不兼容，而 CI 只跑 SQLite ⇒ 跨方言炸点会被
// 藏起来。改为调用方在 Go 侧用模型字段过滤（见 bootstrap/expiry_reminder.go）。
func (r *FileCodeRepository) ListExpiringForReminder(ctx context.Context, now time.Time, advance time.Duration, limit int) ([]*model.FileCode, error) {
	if limit <= 0 {
		limit = ExpiryReminderCandidateLimit
	}
	var items []*model.FileCode
	err := r.db().WithContext(ctx).
		Where("user_id IS NOT NULL").
		Where("status = ?", model.StatusNormal).
		Where("expired_count <> 0").
		Where("expired_at IS NOT NULL AND expired_at > ? AND expired_at <= ?", now, now.Add(advance)).
		Where("expiry_reminded_at IS NULL").
		Order("expired_at ASC").
		Limit(limit).
		Find(&items).Error
	return items, err
}

// MarkExpiryReminded 抢「已提醒」标记，返回**本次是否抢到**。
//
// 用条件更新（`expiry_reminded_at IS NULL`）而不是无条件写：多副本部署下每个
// 副本都会扫到同一批分享，谁先 UPDATE 成功谁负责发，其余副本 RowsAffected=0
// 直接跳过。少了这个条件就是 N 倍重复打扰（且用户无法关掉其中任何一条）。
func (r *FileCodeRepository) MarkExpiryReminded(ctx context.Context, id uint, at time.Time) (bool, error) {
	res := r.db().WithContext(ctx).Model(&model.FileCode{}).
		Where("id = ? AND expiry_reminded_at IS NULL", id).
		Update("expiry_reminded_at", at)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// ClearExpiryReminded 清除标记。
//
// 两个调用方，语义不同但动作相同：
//   - 发送失败后回滚（抢了标记却没发出去 ⇒ 下一轮必须能重试，否则这条提醒就永久丢了）；
//   - 过期时间被延长（已提醒过的分享延期后是「新的到期日」，应当再提醒一次）。
func (r *FileCodeRepository) ClearExpiryReminded(ctx context.Context, id uint) error {
	return r.db().WithContext(ctx).Model(&model.FileCode{}).
		Where("id = ?", id).
		Update("expiry_reminded_at", nil).Error
}
