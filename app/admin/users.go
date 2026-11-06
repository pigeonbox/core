// users.go 用户管理簇：用户列表/筛选/删除/密码重置/状态变更。
package admin

import (
	"context"
	"fmt"

	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
)

// GetUsers 获取用户列表
func (s *Service) GetUsers(ctx context.Context, page, pageSize int) ([]*model.UserResp, int64, error) {
	users, total, err := s.userRepo.List(ctx, page, pageSize)
	if err != nil {
		return nil, 0, err
	}

	resps := make([]*model.UserResp, len(users))
	for i, user := range users {
		resps[i] = user.ToResp()
	}
	s.applyEffectiveStorage(ctx, resps)

	return resps, total, nil
}

// applyEffectiveStorage 把列表里的「存储占用」覆盖为 file_codes 的**实时聚合**。
//
// 为什么不用 users.total_storage：那是个只增不减的漂移计数器——删分享、管理员
// 删用户、换账号重传都不会回退它（线上实录：某用户计数器 4.9GB 而 file_codes
// 里 0 行；另一用户计数器 8MB 实际 0）。配额判定 CheckQuota 与用户侧 GetStats
// 早已改用实时聚合 ⇒ 列表再读计数器就是**两套真相**：列表说超配额、上传却放行。
//
// 聚合失败不阻塞列表（保留计数器原值并留日志），避免统计抖动把用户页打崩。
func (s *Service) applyEffectiveStorage(ctx context.Context, resps []*model.UserResp) {
	if len(resps) == 0 {
		return
	}
	ids := make([]uint, 0, len(resps))
	for _, r := range resps {
		ids = append(ids, r.ID)
	}
	sizes, err := s.fileCodeRepo.GetTotalSizesByUserIDs(ctx, ids)
	if err != nil {
		logger.CtxWarn(ctx, "aggregate user storage failed, keep counter value",
			"users", len(ids), "err", err)
		return
	}
	for _, r := range resps {
		r.TotalStorage = sizes[r.ID] // 无分享记录 ⇒ 0
	}
}

// DeleteUser 删除用户（级联软删其全部分享记录；此前"先删用户文件"逻辑被注释）
func (s *Service) DeleteUser(ctx context.Context, userID uint) error {
	user, err := s.userRepo.GetByID(ctx, userID)
	if err != nil {
		return err
	}

	// 1. 级联软删用户的分享记录（不删物理文件——管理员可从回收站追溯）
	if err := s.fileCodeRepo.DeleteByUserID(ctx, userID); err != nil {
		logger.CtxWarn(ctx, "soft delete user shares failed", "user_id", userID, "err", err)
	}

	// 2. 删除用户记录
	err = s.userRepo.Delete(ctx, userID)
	s.logAdminOperation(ctx, "user.delete",
		fmt.Sprintf("user %d (username=%s) deleted", userID, user.Username), err == nil)
	return err
}

// ============ 管理端增强操作（transport handler 经此下沉，不直连 DAO） ============

// UserListFilter 带筛选的用户列表条件（管理端用户管理）。
type UserListFilter struct {
	Keyword  string
	Status   string
	Role     string
	Page     int
	PageSize int
}

// ListUsersFiltered 带筛选的用户列表（keyword/status/role + 分页）。
func (s *Service) ListUsersFiltered(ctx context.Context, f UserListFilter) ([]*model.UserResp, int64, error) {
	users, total, err := s.userRepo.ListFiltered(ctx, dao.UserFilter{
		Keyword:  f.Keyword,
		Status:   f.Status,
		Role:     f.Role,
		Page:     f.Page,
		PageSize: f.PageSize,
	})
	if err != nil {
		return nil, 0, err
	}
	resps := make([]*model.UserResp, len(users))
	for i, u := range users {
		resps[i] = u.ToResp()
	}
	s.applyEffectiveStorage(ctx, resps)

	return resps, total, nil
}

// ResetUserPassword 管理员重置用户密码（写入已哈希口令）。
func (s *Service) ResetUserPassword(ctx context.Context, userID uint, passwordHash string) error {
	if err := s.userRepo.UpdatePasswordHash(ctx, userID, passwordHash); err != nil {
		return err
	}
	// 重置密码 bump 会话纪元：被盗会话即时失效（2026-10-05 审计 P3）
	if berr := s.userRepo.BumpSessionEpoch(ctx, userID); berr == nil {
		middleware.InvalidateIdentity(userID)
	}
	return nil
}

// UpdateUserStatus 更新用户状态
func (s *Service) UpdateUserStatus(ctx context.Context, userID uint, status string) error {
	user, err := s.userRepo.GetByID(ctx, userID)
	if err != nil {
		return err
	}

	statusChanged := user.Status != status
	user.Status = status
	err = s.userRepo.Update(ctx, user)
	// 封禁/停用 bump 会话纪元：既有 JWT 即时失效（2026-10-05 审计 P2，
	// 此前被封用户 token 可用到自然过期且 refresh 可无限续期）
	if err == nil && statusChanged {
		if berr := s.userRepo.BumpSessionEpoch(ctx, userID); berr == nil {
			middleware.InvalidateIdentity(userID)
		}
	}
	s.logAdminOperation(ctx, "user.update_status",
		fmt.Sprintf("user %d (username=%s) status -> %s", userID, user.Username, status), err == nil)
	return err
}
