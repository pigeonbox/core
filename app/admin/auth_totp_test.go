package admin

// 评审 S01 验收：任意管理员登录入口不能绕过已启用的 MFA。
//
// 背景：TOTP 只在 user.finishLogin 里强制，POST /admin/login 此前完全不查
// TotpEnabled ⇒ 开了 MFA 的管理员绕后台登录即绕二次验证。本文件把
// 「缺码 40012 / 错码 40013 / 对码放行 / 未开不受影响 / 口令先于 MFA」
// 五条语义钉死；错误形态（ErrTotpRequired/ErrTotpInvalid）由 handler 映射
// 成 errcode.CodeTotpRequired(40012, 不计锁定) / CodeTotpInvalid(40013, 计锁定)。

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/pkg/totp"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// newTotpTestService 带 users 表的 admin 服务（登录链路只碰 userRepo）。
func newTotpTestService(t *testing.T) *Service {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.User{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	// :memory: 每条连接独立库 ⇒ 钉死单连接
	sqlDB.SetMaxOpenConns(1)
	return NewService(g)
}

// seedTotpAdmin 建一个指定用户名的管理员；totpEnabled 控制 MFA 开关。
func seedTotpAdmin(t *testing.T, svc *Service, username string, totpEnabled bool) (*model.User, string) {
	t.Helper()
	const password = "admin-passw0rd!"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	require.NoError(t, err)
	secret, err := totp.GenerateSecret()
	require.NoError(t, err)
	u := &model.User{
		Username: username, Email: username + "@test.local",
		PasswordHash: string(hash), Role: "admin", Status: "active",
		TotpEnabled: totpEnabled, TotpSecret: secret,
	}
	require.NoError(t, svc.userRepo.Create(context.Background(), u))
	return u, password
}

// totpCodeAt 当前时刻的有效码（失败即让测试失败，不静默拿空串）。
func totpCodeAt(t *testing.T, secret string) string {
	t.Helper()
	c, err := totp.Code(secret, time.Now())
	require.NoError(t, err)
	return c
}

// TestAdminLogin_TOTPEnforced 管理员登录强制 MFA 的五条验收语义。
func TestAdminLogin_TOTPEnforced(t *testing.T) {
	svc := newTotpTestService(t)
	ctx := context.Background()

	t.Run("①缺码与空白码报ErrTotpRequired", func(t *testing.T) {
		u, pw := seedTotpAdmin(t, svc, "mfa_missing", true)
		_, err := svc.GenerateTokenForAdmin(ctx, u.Username, pw, "")
		require.ErrorIs(t, err, ErrTotpRequired, "缺码必须 ErrTotpRequired（handler 映射 40012，不计锁定）")
		// 空白码同缺码：TrimSpace 后为空即「没带码」，不能拿 " " 骗过闸门
		_, err = svc.GenerateTokenForAdmin(ctx, u.Username, pw, "   ")
		require.ErrorIs(t, err, ErrTotpRequired, "纯空白码必须同缺码处理")
	})

	t.Run("②错码报ErrTotpInvalid", func(t *testing.T) {
		u, pw := seedTotpAdmin(t, svc, "mfa_wrong", true)
		_, err := svc.GenerateTokenForAdmin(ctx, u.Username, pw, "000000")
		require.ErrorIs(t, err, ErrTotpInvalid, "错码必须 ErrTotpInvalid（handler 映射 40013，计入锁定桶）")
	})

	t.Run("③正确码成功签发", func(t *testing.T) {
		u, pw := seedTotpAdmin(t, svc, "mfa_ok", true)
		token, err := svc.GenerateTokenForAdmin(ctx, u.Username, pw, totpCodeAt(t, u.TotpSecret))
		require.NoError(t, err, "正确码必须成功签发")
		assert.NotEmpty(t, token)
	})

	t.Run("④未开TOTP的管理员带不带码都能登", func(t *testing.T) {
		u, pw := seedTotpAdmin(t, svc, "mfa_off", false)
		token, err := svc.GenerateTokenForAdmin(ctx, u.Username, pw, "")
		require.NoError(t, err, "存量未开 MFA 的管理员不受影响（空码）")
		assert.NotEmpty(t, token)
		// 带码也不该被拦：TotpEnabled=false 时码根本不进校验
		token, err = svc.GenerateTokenForAdmin(ctx, u.Username, pw, "000000")
		require.NoError(t, err, "未开 MFA 时带任意码也放行")
		assert.NotEmpty(t, token)
	})

	t.Run("⑤密码错时即便带正确码也失败且不是TOTP哨兵", func(t *testing.T) {
		u, _ := seedTotpAdmin(t, svc, "mfa_badpass", true)
		_, err := svc.GenerateTokenForAdmin(ctx, u.Username, "wrong-password", totpCodeAt(t, u.TotpSecret))
		require.Error(t, err, "口令错必须失败")
		// 顺序：口令 → 角色/状态 → MFA。口令没过不许走到二验分支——
		// 否则「先回40012 再试码」能把口令爆破伪装成 MFA 流程。
		require.NotErrorIs(t, err, ErrTotpRequired, "口令错不得伪装成缺码（40012）")
		require.NotErrorIs(t, err, ErrTotpInvalid, "口令错不得伪装成错码（40013）")
	})
}
