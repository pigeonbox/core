package user

// 登录设备管理（5.2.0）。
//
// 「我有哪些设备在线」在无状态 JWT 下**无法回答**——JWT 只在客户端手里，
// 服务端不留痕。所以登录时登记一行会话，这里读这张表。
//
// 语义边界（必须与「退出登录」区分清楚）：
//
//	· 撤销单条会话 = 那台设备下次请求即 401（当前设备可保留）
//	· 退出所有设备 = 撤销全部 + 会话纪元 +1 ⇒ **当前设备也会被踢**
//	  （纪元 +1 是为了兜住升级前签发、没有 sid 的老令牌；不 +1 就等于
//	  「老令牌永远踢不掉」）
//
// API Key **不受影响**：它是长期程序凭据，语义与 admin 口令重置一致
// （那边也只失效 JWT、保留 Key）。整合进来会让「我以为踢掉了 CI」变成事故。

import (
	"context"
	"errors"
	"time"

	"github.com/pigeonbox/core/pkg/middleware"
)

// SessionItem 一条登录设备（对外形状，handler 转 contracts 模型）。
type SessionItem struct {
	ID         uint
	IP         string
	UserAgent  string
	CreatedAt  time.Time
	LastSeenAt time.Time
	// Current 本次请求所用的会话（前端据此禁掉「踢掉自己」的按钮）。
	Current bool
}

// ListSessions 该用户仍然有效的登录设备（最近活跃在前）。
func (s *Service) ListSessions(ctx context.Context, userID uint, currentSID string) ([]*SessionItem, error) {
	if s.sessionRepo == nil {
		return nil, errors.New("会话管理不可用")
	}
	rows, err := s.sessionRepo.ListActive(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]*SessionItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, &SessionItem{
			ID:         r.ID,
			IP:         r.IP,
			UserAgent:  r.UserAgent,
			CreatedAt:  r.CreatedAt,
			LastSeenAt: r.LastSeenAt,
			Current:    currentSID != "" && r.SessionID == currentSID,
		})
	}
	return out, nil
}

// RevokeSession 踢掉某一条设备。
//
// 归属判定在 DAO 的 WHERE 里（user_id + id + 未撤销）⇒ 不存在「查完再改」
// 的越权窗口；末了必须 InvalidateIdentity，否则身份缓存里那句
// 「该 sid 仍然有效」最多还要活 30 秒。
func (s *Service) RevokeSession(ctx context.Context, userID, id uint) error {
	if s.sessionRepo == nil {
		return errors.New("会话管理不可用")
	}
	ok, err := s.sessionRepo.Revoke(ctx, userID, id, time.Now())
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("会话不存在或已退出")
	}
	middleware.InvalidateIdentity(userID)
	return nil
}

// RevokeAllSessions 退出所有设备（含当前设备），返回受影响行数。
//
// 顺序不可换：先撤会话行、再 bump 纪元。反过来的话，若撤行失败，用户会看到
// 「已退出所有设备」但老 sid 令牌还活着（因为纪元 +1 已经把 token 里的
// 旧纪元作废——所以这里其实两边都失效，安全性没丢；但报错必须如实，
// 不能让用户以为操作成功）。
func (s *Service) RevokeAllSessions(ctx context.Context, userID uint) (int64, error) {
	if s.sessionRepo == nil {
		return 0, errors.New("会话管理不可用")
	}
	n, err := s.sessionRepo.RevokeAll(ctx, userID, 0, time.Now())
	if err != nil {
		return 0, err
	}
	if err := s.repo.BumpSessionEpoch(ctx, userID); err != nil {
		return n, err
	}
	middleware.InvalidateIdentity(userID)
	return n, nil
}

// RevokeSessionBySID 按 sid 撤销（登出时用：只踢自己这一条）。
// 登出路径本来就是幂等的，未命中不报错。
func (s *Service) RevokeSessionBySID(ctx context.Context, sid string) {
	if s.sessionRepo == nil || sid == "" {
		return
	}
	_ = s.sessionRepo.RevokeBySessionID(ctx, sid, time.Now())
}
