package setup

import (
	"context"
	"errors"
	"fmt"

	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// InitializeSystemReq 系统初始化请求
type InitializeSystemReq struct {
	AdminUsername string
	AdminPassword string
	AdminEmail    string
}

type Service struct {
	userRepo *dao.UserRepository
}

// OnInitialized 首启成功后的回调（由装配层注入）。
//
// 用途：首启面关闭时做**收尾**动作——当前唯一消费方是 bootstrap 清理
// 一次性首启令牌（进程内 + 数据目录下的 .setup_token 文件）。
//
// 为何用钩子而不是直接 import bootstrap：app/setup 是领域服务，反向依赖
// 装配层会成环（bootstrap 已 import app/setup）。
var OnInitialized func()

func NewService(gormDB *gorm.DB) *Service {
	return &Service{
		userRepo: dao.NewUserRepository(gormDB),
	}
}

// IsSystemInitialized 检查系统是否已初始化
// 通过检查是否存在管理员用户来判断
func (s *Service) IsSystemInitialized(ctx context.Context) (bool, error) {
	count, err := s.userRepo.CountAdminUsers(ctx)
	if err != nil {
		return false, fmt.Errorf("查询管理员用户失败: %w", err)
	}
	return count > 0, nil
}

// InitializeSystem 初始化系统，创建第一个管理员
func (s *Service) InitializeSystem(ctx context.Context, req *InitializeSystemReq) error {
	// 检查是否已存在管理员
	count, err := s.userRepo.CountAdminUsers(ctx)
	if err != nil {
		return fmt.Errorf("检查管理员状态失败: %w", err)
	}
	if count > 0 {
		return errors.New("系统已初始化，禁止重复初始化")
	}

	// 检查用户名是否已存在
	if user, _ := s.userRepo.GetByUsername(ctx, req.AdminUsername); user != nil {
		return errors.New("用户名已存在")
	}

	// 检查邮箱是否已存在
	if user, _ := s.userRepo.GetByEmail(ctx, req.AdminEmail); user != nil {
		return errors.New("邮箱已存在")
	}

	// 哈希密码
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(req.AdminPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("密码哈希失败: %w", err)
	}

	// 创建管理员用户（事务内"无管理员才创建"，消除 check-then-act 并发窗口——
	// 2026-10-05 审计 P2：初始化空窗期未认证者可抢建管理员）
	admin := &model.User{
		Username:      req.AdminUsername,
		Email:         req.AdminEmail,
		PasswordHash:  string(hashedPassword),
		Nickname:      req.AdminUsername, // 默认使用用户名作为昵称
		Role:          "admin",
		Status:        "active",
		EmailVerified: true,
	}

	created, err := s.userRepo.CreateFirstAdminIfNoAdmin(ctx, admin)
	if err != nil {
		return fmt.Errorf("创建管理员失败: %w", err)
	}
	if !created {
		return errors.New("系统已初始化，禁止重复初始化")
	}

	// 首启成功 ⇒ 通知装配层收尾（清理一次性令牌等）。
	if OnInitialized != nil {
		OnInitialized()
	}

	return nil
}
