// auth.go 认证职责簇：管理员登录凭据校验与 JWT 签发（GenerateTokenForAdmin 属认证职责，单独成文件）。
package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pigeonbox/core/app/authn"
	"github.com/pigeonbox/core/pkg/auth"
	"github.com/pigeonbox/core/pkg/logger"
	"golang.org/x/crypto/bcrypt"
)

// 登录失败的类型化错误——handler 据此映射 40012/40013（40012 不计锁定）。
var (
	// ErrTotpRequired 账号已开启二验但本次登录未提供验证码（40012）。
	//
	// 这是**流程信号**不是失败：口令已经对了，只缺码 ⇒ 前端据此弹第二个输入框；
	// 计入失败会让「输一次码」把管理员锁在门外。
	ErrTotpRequired = errors.New("需要二次验证码")
	// ErrTotpInvalid 二次验证码/恢复码错误（40013）——真失败，计入锁定桶。
	ErrTotpInvalid = errors.New("二次验证码错误")
	// ErrInternal 登录链路内部故障（DB/签发）的包装标记——handler 据此走
	// NewInternalError 脱敏出网（core#114 A6b：驱动/SQL 文本不随响应出去）。
	ErrInternal = errors.New("登录内部错误")
)

// GenerateTokenForAdmin 生成管理员登录 token。
//
// totpCode：二次验证码，与 /user/login 的 totp_code 同语义——账号已开 MFA 时
// 缺码 → ErrTotpRequired、错码 → ErrTotpInvalid；判据与 user.finishLogin 共用
// authn.VerifySecondFactor（评审 S01：admin 登录此前不查 TotpEnabled，开 MFA
// 的管理员绕 /admin/login 即绕 MFA）。
func (s *Service) GenerateTokenForAdmin(ctx context.Context, username, password, totpCode string) (string, error) {
	// 查找用户
	user, err := s.userRepo.GetByUsername(ctx, username)
	if err != nil {
		return "", errors.New("用户名或密码错误")
	}

	// 验证密码（2026-10-05 审计 P3：先比密码再判角色——原顺序下错误信息
	// "权限不足" vs "用户名或密码错误" 可区分"该用户名是管理员"，构成
	// 管理员用户名探测预言机）
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return "", errors.New("用户名或密码错误")
	}

	// 检查是否为管理员
	if user.Role != "admin" {
		return "", errors.New("权限不足")
	}

	// 检查用户状态
	if user.Status != "active" {
		return "", errors.New("用户已被禁用")
	}

	// MFA（评审 S01）：与 user.finishLogin 同判据、同错误语义。
	// 顺序固定在口令/角色/状态**之后**——口令没过不许回 40012/40013，
	// 否则「先弹二验框再试码」能把口令爆破伪装成 MFA 流程。
	if user.TotpEnabled {
		code := strings.TrimSpace(totpCode)
		if code == "" {
			return "", ErrTotpRequired // 40012：口令已对、只缺码——不计失败
		}
		ok, err := authn.VerifySecondFactor(ctx, s.userRepo, user.ID, code)
		if err != nil {
			// 内部故障：原文进服务端日志，对外只留 ErrInternal 标记
			//（handler 走 NewInternalError 脱敏，core#114 A6b）。
			logger.CtxError(ctx, "admin totp verify failed", "user_id", user.ID, "err", err)
			return "", fmt.Errorf("%w: 二次验证校验失败: %w", ErrInternal, err)
		}
		if !ok {
			return "", ErrTotpInvalid // 40013：计入锁定桶
		}
	}

	// 生成 token (24小时过期)
	token, err := auth.GenerateToken(user.ID, user.Username, "admin")
	if err != nil {
		return "", errors.New("生成 token 失败")
	}

	// 更新最后登录时间
	now := time.Now()
	user.LastLoginAt = &now
	_ = s.userRepo.Update(ctx, user)

	return token, nil
}
