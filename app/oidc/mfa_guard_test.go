// wave1 A1 → core#180：OIDC 对**已启用 MFA 的管理员**不直接签发正式会话，
// 改发 pending-2FA 半会话，二验换发后才是正式会话。
//
// 漏洞形态（origin/main）：ExchangeCallback 按 DB 角色直接 auth.GenerateToken
// ⇒ OIDC 回调零二验检查 ⇒ 已开 TOTP 的管理员从 OIDC 入口登录即绕过 MFA
// （authn.VerifySecondFactor 全仓仅本地 admin 登录与 user TOTP 两处调用）。
//
// #187 的临时方案是「拒绝签发」（ErrAdminMFARequired）——其固有边：无口令的
// OIDC 原生管理员（JIT 号被提为 admin 后开 TOTP）被挡在门外且无本地口令可登
// ⇒ 锁死。本文件钉的是 proper 修法（#180）的判据：
//  1. admin + TotpEnabled=true ⇒ 半会话（MFAPending=true），非正式会话；
//  2. admin + TotpEnabled=false ⇒ 正式会话（回归防误杀）；
//  3. user + TotpEnabled=true ⇒ 正式会话（拦截面只限 admin）；
//  4. 换发：对码换正式会话 + 半会话吊销；错码 ErrMFACodeInvalid；非 pending /
//     已吊销半会话拒换。
//
// 负对照（改错转红）：把第 4 步改回 auth.GenerateToken 直签 ⇒ ① 的
// MFAPending 断言转红（拿到正式会话）；把换发的 VerifySecondFactor 摘掉 ⇒
// ②③ 的错码/重放断言转红。
package oidc

import (
	"context"
	"testing"
	"time"

	"github.com/oekit/pkg/secretx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/pkg/auth"
	"github.com/pigeonbox/core/pkg/totp"
	"github.com/pigeonbox/core/repo/db/model"
)

// mfaCaseSvc 构造「既有账号已按 sub 绑定」的服务与库（fakeIdP 恒回
// sub=user-sub-42），供签发面与换发面用例复用。
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

// TestOIDC_AdminWithMFAIssuesPendingSession（半会话签发）：admin + TotpEnabled
// 走 OIDC ⇒ 拿到 pending-2FA 半会话（非正式会话、非拒绝），role 保持 admin。
func TestOIDC_AdminWithMFAIssuesPendingSession(t *testing.T) {
	secret, err := totp.GenerateSecret()
	require.NoError(t, err)
	svc, tokHits := mfaCaseSvc(t, &model.User{
		Username: "root", Email: "root@example.com", PasswordHash: "x",
		Role: "admin", TotpEnabled: true, TotpSecret: secret,
	})

	token, err := mfaCallback(svc)

	require.NoError(t, err,
		"admin+MFA 经 OIDC 应拿到半会话（拒绝签发会锁死无口令 OIDC 管理员，core#180）")
	require.NotEmpty(t, token, "半会话也是 token（回调 Cookie 承载）")
	claims, perr := auth.ParseToken(token)
	require.NoError(t, perr)
	assert.True(t, claims.MFAPending, "必须带 pending-2FA 半会话声明")
	assert.Equal(t, "admin", claims.Role, "role 按 DB 原样保留（换发后仍需过 AdminMiddleware 角色关）")
	assert.Equal(t, 1, *tokHits, "token 交换/userinfo 已发生（签发点在匹配后，非更早）")
}

// TestOIDC_AdminWithoutMFALetsThrough（放）：admin + TotpEnabled=false ⇒ 正式
// 会话（未开 MFA 的 admin 走 OIDC 属支持路径，防误杀回归）。
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
	assert.False(t, claims.MFAPending, "未开 MFA 必须是正式会话")
	assert.Equal(t, "admin", claims.Role, "应按 DB 角色正常签发 admin 会话")
}

// TestOIDC_UserWithMFALetsThrough（放）：user + TotpEnabled=true ⇒ 正式会话
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
	assert.False(t, claims.MFAPending, "user 半会话没有业务意义（二验面只限 admin）")
	assert.Equal(t, "user", claims.Role)
}

// TestOIDC_ExchangeMFASession 换发半程判据（core#180）。
func TestOIDC_ExchangeMFASession(t *testing.T) {
	secret, err := totp.GenerateSecret()
	require.NoError(t, err)
	svc, _ := mfaCaseSvc(t, &model.User{
		Username: "root", Email: "root@example.com", PasswordHash: "x",
		Role: "admin", TotpEnabled: true, TotpSecret: secret,
	})
	ctx := context.Background()

	half, err := mfaCallback(svc)
	require.NoError(t, err)

	// ① 错码 → ErrMFACodeInvalid，且失败不吊销半会话（失败不惩罚，可重试）
	_, err = svc.ExchangeMFASession(ctx, half, "000000")
	require.ErrorIs(t, err, ErrMFACodeInvalid)

	// ② 对码 → 正式会话（MFAPending 归零，role=admin）
	code, cerr := totp.Code(secret, time.Now())
	require.NoError(t, cerr)
	full, err := svc.ExchangeMFASession(ctx, half, code)
	require.NoError(t, err, "对码必须换发正式会话")
	claims, perr := auth.ParseToken(full)
	require.NoError(t, perr)
	assert.False(t, claims.MFAPending, "换发产物必须是正式会话")
	assert.Equal(t, "admin", claims.Role)

	// ③ 半会话一次性：换发后重放 → 拒（防持半会话无限换新）
	_, err = svc.ExchangeMFASession(ctx, half, code)
	require.ErrorIs(t, err, ErrMFASessionInvalid, "半会话换发后必须吊销")

	// ④ 非 pending 的正式会话不得走换发口
	normal, nerr := auth.GenerateToken(1, "someone", "user")
	require.NoError(t, nerr)
	_, err = svc.ExchangeMFASession(ctx, normal, code)
	require.ErrorIs(t, err, ErrMFASessionInvalid, "正式会话不得经换发口重签")
}
