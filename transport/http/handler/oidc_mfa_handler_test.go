// core#180 wire 级判据：半会话的回跳标记 + 换发端点 /api/v1/user/oidc/mfa。
//
// 负对照（改错转红）：
//   - oidcFrontendRedirect 丢掉 pending 分支 ⇒ mfa_pending=1 断言转红；
//   - OIDCMFAComplete 摘掉二验（直接换发）⇒ 错码用例转红（200 而非 40013）；
//   - 换发口不验 MFAPending ⇒ 「正式会话不得换发」401 断言转红。
package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	oidcApp "github.com/pigeonbox/core/app/oidc"
	"github.com/pigeonbox/core/pkg/auth"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/pkg/totp"
	"github.com/pigeonbox/core/repo/db/model"
)

// newOIDCMFAEnv 启用的 OIDC 服务 + 开 TOTP 的 admin（按 sub 绑定）。
// 返回 (服务, 用户ID, TOTP密钥)。
func newOIDCMFAEnv(t *testing.T) (*oidcApp.Service, uint, string) {
	t.Helper()
	auth.SetJWTSecret("oidc-mfa-handler-test-secret-0123456789")
	t.Cleanup(func() { auth.SetJWTSecret("") })

	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.User{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	secret, err := totp.GenerateSecret()
	require.NoError(t, err)
	hash, err := bcrypt.GenerateFromPassword([]byte("x"), bcrypt.MinCost)
	require.NoError(t, err)
	u := &model.User{
		Username: "root", Email: "root@example.com",
		PasswordHash: string(hash), Role: "admin", Status: "active",
		OidcSub: "sub-mfa-1", TotpEnabled: true, TotpSecret: secret,
	}
	require.NoError(t, g.Create(u).Error)

	svc := oidcApp.NewService(g, oidcApp.Config{
		Enabled: true, Issuer: "https://idp.invalid", ClientID: "pb-client",
	})
	SetOIDCService(svc)
	t.Cleanup(func() { SetOIDCService(nil) })
	return svc, u.ID, secret
}

// postMFA 打一次真实 POST /api/v1/user/oidc/mfa。
func postMFA(t *testing.T, h *server.Hertz, cookie, body string) (int, map[string]any, string) {
	t.Helper()
	w := ut.PerformRequest(h.Engine, http.MethodPost, "/api/v1/user/oidc/mfa",
		&ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"},
		ut.Header{Key: "Cookie", Value: cookie})
	var m map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	return w.Code, m, w.Result().Header.Get("Set-Cookie")
}

// TestOIDCFrontendRedirect_MFAPendingFlag 回跳判据：半会话挂 mfa_pending=1，
// 正式会话不挂。
func TestOIDCFrontendRedirect_MFAPendingFlag(t *testing.T) {
	svc, uid, _ := newOIDCMFAEnv(t)

	half, err := auth.GeneratePendingMFAToken(uid, "root", "admin")
	require.NoError(t, err)
	loc := oidcFrontendRedirect(svc, half)
	assert.Contains(t, loc, "mfa_pending=1", "半会话回跳必须挂二验标记（前端据此渲染二验页）")

	full, err := auth.GenerateToken(uid, "root", "admin")
	require.NoError(t, err)
	loc = oidcFrontendRedirect(svc, full)
	assert.NotContains(t, loc, "mfa_pending", "正式会话回跳不得挂二验标记")
}

// TestOIDCMFAComplete_ExchangeFlow 换发端点的 wire 判据。
func TestOIDCMFAComplete_ExchangeFlow(t *testing.T) {
	_, uid, secret := newOIDCMFAEnv(t)
	code, err := totp.Code(secret, time.Now())
	require.NoError(t, err)

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.POST("/api/v1/user/oidc/mfa", OIDCMFAComplete)

	half, err := auth.GeneratePendingMFAToken(uid, "root", "admin")
	require.NoError(t, err)
	cookie := middleware.SessionCookieName + "=" + half

	t.Run("①缺码 ⇒ 40012", func(t *testing.T) {
		st, body, _ := postMFA(t, h, cookie, "{}")
		require.Equal(t, http.StatusBadRequest, st)
		require.EqualValues(t, 40012, body["code"], "缺码必须 40012（与 /admin/login 同语义）")
	})

	t.Run("②错码 ⇒ 40013", func(t *testing.T) {
		st, body, _ := postMFA(t, h, cookie, "{\"code\":\"000000\"}")
		require.Equal(t, http.StatusBadRequest, st)
		require.EqualValues(t, 40013, body["code"], "错码必须 40013")
	})

	t.Run("③无半会话 ⇒ 401", func(t *testing.T) {
		st, body, _ := postMFA(t, h, "", "{\"code\":\""+code+"\"}")
		require.Equal(t, http.StatusUnauthorized, st, "body=%v", body)
	})

	t.Run("④正式会话走换发口 ⇒ 401（不得重签）", func(t *testing.T) {
		full, ferr := auth.GenerateToken(uid, "root", "admin")
		require.NoError(t, ferr)
		st, body, _ := postMFA(t, h, middleware.SessionCookieName+"="+full, "{\"code\":\""+code+"\"}")
		require.Equal(t, http.StatusUnauthorized, st, "body=%v", body)
	})

	t.Run("⑤对码 ⇒ 200 + 新会话 Cookie（正式）", func(t *testing.T) {
		st, body, setCookie := postMFA(t, h, cookie, "{\"code\":\""+code+"\"}")
		require.Equal(t, http.StatusOK, st, "body=%v", body)
		require.EqualValues(t, 0, body["code"], "body=%v", body)
		require.Contains(t, setCookie, middleware.SessionCookieName+"=", "必须下发新会话 Cookie")

		// 新 Cookie 必须是**正式**会话（MFAPending=false）
		idx := strings.Index(setCookie, middleware.SessionCookieName+"=")
		raw := setCookie[idx+len(middleware.SessionCookieName)+1:]
		if i := strings.Index(raw, ";"); i >= 0 {
			raw = raw[:i]
		}
		claims, perr := auth.ParseToken(raw)
		require.NoError(t, perr, "新 Cookie 必须可解析")
		assert.False(t, claims.MFAPending, "换发产物必须是正式会话")
		assert.Equal(t, "admin", claims.Role)
	})
}
