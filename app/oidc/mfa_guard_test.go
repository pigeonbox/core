// wave1 A1（终审 must-fix）：OIDC 不得为**已启用 MFA 的管理员**签发会话。
//
// 漏洞形态（origin/main）：ExchangeCallback 按 DB 角色直接
// auth.GenerateToken ⇒ OIDC 回调零二验检查 ⇒ 已开 TOTP 的管理员从 OIDC 入口
// 登录即绕过 MFA（authn.VerifySecondFactor 全仓仅本地 admin 登录与 user TOTP
// 两处调用）。计划 S01 头条验收：任意管理员登录入口不能绕过已启用的 MFA。
//
// 裁决：签发前拦截（admin && TotpEnabled 两条件同时成立）⇒ 返回
// ErrAdminMFARequired，不签发 token；未开 MFA 的 admin 与 user 角色不受影响。
//
// 四条负向测试（计划硬规则，缺一不可）：
//  1. 拒：admin + TotpEnabled=true ⇒ sentinel 且零签发；
//  2. 放：admin + TotpEnabled=false ⇒ 正常签发（回归防误杀）；
//  3. 放：user + TotpEnabled=true ⇒ 正常签发（拦截面只限 admin）；
//  4. 回调层映射在 transport/http/handler/oidc_error_test.go。
package oidc

import (
	"context"
	"testing"
	"time"

	"github.com/oekit/pkg/secretx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/pkg/auth"
	"github.com/pigeonbox/core/repo/db/model"
)

// mfaCaseSvc 构造「既有账号已按 sub 绑定」的服务与库（fakeIdP 恒回
// sub=user-sub-42），供三条签发面用例复用。
func mfaCaseSvc(t *testing.T, user *model.User) (*Service, *int) {
	t.Helper()
	oidcDB := newOIDCDB(t)
	auth.SetJWTSecret("oidc-test-secret-0123456789abcdef")
	srv, tokHits := fakeIdP(t)
	svc := NewService(oidcDB, Config{
		Enabled: true, Issuer: srv.URL,
		ClientID: "c", ClientSecret: secretx.NewSecureString("s"),
	})
	user.OidcSub = "user-sub-42"
	user.Status = "active"
	require.NoError(t, oidcDB.Create(user).Error)
	return svc, tokHits
}

// mfaCallback 走一次完整回调（state 用服务自己的 HMAC 签，nonce 配对）。
func mfaCallback(svc *Service) (string, error) {
	nonce := "mfa-nonce"
	return svc.ExchangeCallback(context.Background(), "http://pb.local", "the-code",
		svc.signState(time.Now().Unix(), nonce), nonce)
}

// TestOIDC_AdminWithMFABeforeIssuance（拒）：admin + TotpEnabled=true 走 OIDC
// 匹配 ⇒ 返回 ErrAdminMFARequired，且**未签发 token**（断言到具体 sentinel +
// 空 token，不只断言「有错」——笼统有错会把 state/网络失败误判成拦对了）。
//
// 负对照（改错转红）：删掉 ExchangeCallback 的 admin&&TotpEnabled 拦截 ⇒
// ErrorIs 转红（err=nil）；拦截只返回错误但仍 GenerateToken ⇒ Empty 转红。
func TestOIDC_AdminWithMFABeforeIssuance(t *testing.T) {
	svc, tokHits := mfaCaseSvc(t, &model.User{
		Username: "root", Email: "root@example.com", PasswordHash: "x",
		Role: "admin", TotpEnabled: true,
	})

	token, err := mfaCallback(svc)

	require.ErrorIs(t, err, ErrAdminMFARequired,
		"已启用 MFA 的管理员走 OIDC 必须命中专用 sentinel")
	assert.Empty(t, token, "拦截路径不得返回任何 token（签发即绕过 MFA）")
	assert.Equal(t, 1, *tokHits, "token 交换/ userinfo 已发生（拦截点在签发前，非更早）")
}

// TestOIDC_AdminWithoutMFALetsThrough（放）：admin + TotpEnabled=false ⇒ 正常
// 签发（未开 MFA 的 admin 走 OIDC 属支持路径，防误杀回归）。
func TestOIDC_AdminWithoutMFALetsThrough(t *testing.T) {
	svc, _ := mfaCaseSvc(t, &model.User{
		Username: "root", Email: "root@example.com", PasswordHash: "x",
		Role: "admin", TotpEnabled: false,
	})

	token, err := mfaCallback(svc)

	require.NoError(t, err, "未开 MFA 的管理员不得被拦截")
	require.NotEmpty(t, token)
	claims, perr := auth.ParseToken(token)
	require.NoError(t, perr)
	assert.Equal(t, "admin", claims.Role, "应按 DB 角色正常签发 admin 会话")
}

// TestOIDC_UserWithMFALetsThrough（放）：user + TotpEnabled=true ⇒ 正常签发
// （拦截面只限 admin——普通用户开 TOTP 走 OIDC 不受影响）。
func TestOIDC_UserWithMFALetsThrough(t *testing.T) {
	svc, _ := mfaCaseSvc(t, &model.User{
		Username: "alice", Email: "alice@example.com", PasswordHash: "x",
		Role: "user", TotpEnabled: true,
	})

	token, err := mfaCallback(svc)

	require.NoError(t, err, "user 角色开 TOTP 不在拦截面内")
	require.NotEmpty(t, token)
	claims, perr := auth.ParseToken(token)
	require.NoError(t, perr)
	assert.Equal(t, "user", claims.Role)
}
