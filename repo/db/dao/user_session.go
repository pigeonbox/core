package dao

import (
	"context"
	"time"

	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm"
)

// SessionRepository 登录会话表（5.2.0）。构造注入 *gorm.DB（去全局化终态）。
type SessionRepository struct {
	conn *gorm.DB
}

func NewSessionRepository(gormDB *gorm.DB) *SessionRepository {
	return &SessionRepository{conn: gormDB}
}

func (r *SessionRepository) db() *gorm.DB { return r.conn }

func (r *SessionRepository) Create(ctx context.Context, s *model.UserSession) error {
	return r.db().WithContext(ctx).Create(s).Error
}

// ListActive 该用户仍然有效的会话（按最近活跃倒序）。
//
// 同时过滤「已软删」与「已撤销」：列表是给用户看的「哪些设备在线」，
// 把撤销过的行也列出来只会让人以为没踢掉。
func (r *SessionRepository) ListActive(ctx context.Context, userID uint) ([]*model.UserSession, error) {
	var out []*model.UserSession
	err := r.db().WithContext(ctx).
		Where("user_id = ? AND revoked_at IS NULL", userID).
		Order("last_seen_at DESC, id DESC").
		Find(&out).Error
	return out, err
}

func (r *SessionRepository) GetBySessionID(ctx context.Context, sid string) (*model.UserSession, error) {
	var s model.UserSession
	err := r.db().WithContext(ctx).Where("session_id = ?", sid).First(&s).Error
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// ActiveSessionIDs 该用户当前仍然有效的会话 sid 集合（认证回查用）。
//
// 为什么返回 map 而不是 []：消费侧是「这个 sid 还在不在」，map 查找是 O(1)，
// 且能区分「集合存在但为空」（该用户零有效会话 ⇒ 带 sid 的令牌一律拒绝）
// 与「nil」（未装配 ⇒ 跳过会话校验）。
func (r *SessionRepository) ActiveSessionIDs(ctx context.Context, userID uint) (map[string]bool, error) {
	var sids []string
	err := r.db().WithContext(ctx).Model(&model.UserSession{}).
		Where("user_id = ? AND revoked_at IS NULL", userID).
		Pluck("session_id", &sids).Error
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(sids))
	for _, s := range sids {
		out[s] = true
	}
	return out, nil
}

// Touch 更新最近活跃时间（登录与刷新令牌时调用，不在每个请求上写）。
func (r *SessionRepository) Touch(ctx context.Context, sid string, at time.Time) error {
	return r.db().WithContext(ctx).Model(&model.UserSession{}).
		Where("session_id = ?", sid).
		UpdateColumn("last_seen_at", at).Error
}

// Revoke 撤销某个会话；返回是否命中（false = 不属于该用户或已撤销）。
//
// 归属判定放在 WHERE 里（user_id + id + 未撤销）⇒ 不能靠「先查再改」，
// 那有 TOCTOU 窗口，也正是越权回读类缺陷的常见形态。
func (r *SessionRepository) Revoke(ctx context.Context, userID, id uint, at time.Time) (bool, error) {
	res := r.db().WithContext(ctx).Model(&model.UserSession{}).
		Where("id = ? AND user_id = ? AND revoked_at IS NULL", id, userID).
		UpdateColumn("revoked_at", at)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// RevokeAll 撤销该用户全部会话，可保留一条（当前设备）。返回受影响行数。
func (r *SessionRepository) RevokeAll(ctx context.Context, userID uint, keepID uint, at time.Time) (int64, error) {
	q := r.db().WithContext(ctx).Model(&model.UserSession{}).
		Where("user_id = ? AND revoked_at IS NULL", userID)
	if keepID > 0 {
		q = q.Where("id <> ?", keepID)
	}
	res := q.UpdateColumn("revoked_at", at)
	return res.RowsAffected, res.Error
}

// RevokeBySessionID 按 sid 撤销（登出时用：只踢自己这一条）。
func (r *SessionRepository) RevokeBySessionID(ctx context.Context, sid string, at time.Time) error {
	return r.db().WithContext(ctx).Model(&model.UserSession{}).
		Where("session_id = ?", sid).
		UpdateColumn("revoked_at", at).Error
}

// DeleteOlderThan 物理清理过期会话（janitor 用）。
func (r *SessionRepository) DeleteOlderThan(ctx context.Context, before time.Time) (int64, error) {
	res := r.db().WithContext(ctx).Unscoped().
		Where("created_at < ?", before).Delete(&model.UserSession{})
	return res.RowsAffected, res.Error
}

// ---- 找回密码令牌 ----

type PasswordResetRepository struct {
	conn *gorm.DB
}

func NewPasswordResetRepository(gormDB *gorm.DB) *PasswordResetRepository {
	return &PasswordResetRepository{conn: gormDB}
}

func (r *PasswordResetRepository) db() *gorm.DB { return r.conn }

func (r *PasswordResetRepository) Create(ctx context.Context, t *model.PasswordResetToken) error {
	return r.db().WithContext(ctx).Create(t).Error
}

func (r *PasswordResetRepository) GetByHash(ctx context.Context, hash string) (*model.PasswordResetToken, error) {
	var t model.PasswordResetToken
	if err := r.db().WithContext(ctx).Where("token_hash = ?", hash).First(&t).Error; err != nil {
		return nil, err
	}
	return &t, nil
}

// MarkUsed 原子地把令牌标记为已用；返回是否命中。
//
// 用 UPDATE ... WHERE used_at IS NULL 而不是「先查再改」：同一个 token
// 被并发提交两次时，只有一次能拿到 RowsAffected=1 ⇒ 一次性语义成立。
func (r *PasswordResetRepository) MarkUsed(ctx context.Context, id uint, at time.Time) (bool, error) {
	res := r.db().WithContext(ctx).Model(&model.PasswordResetToken{}).
		Where("id = ? AND used_at IS NULL", id).
		UpdateColumn("used_at", at)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// InvalidateAllForUser 作废该用户其余未用令牌（改密后调用：旧链接一律失效）。
func (r *PasswordResetRepository) InvalidateAllForUser(ctx context.Context, userID uint, at time.Time) error {
	return r.db().WithContext(ctx).Model(&model.PasswordResetToken{}).
		Where("user_id = ? AND used_at IS NULL", userID).
		UpdateColumn("used_at", at).Error
}

func (r *PasswordResetRepository) DeleteExpired(ctx context.Context, before time.Time) (int64, error) {
	res := r.db().WithContext(ctx).Unscoped().
		Where("expires_at < ?", before).Delete(&model.PasswordResetToken{})
	return res.RowsAffected, res.Error
}
