package bootstrap

// core#160 管理员 Passkey 四端点的 wire 级判据：字段绑定（username/challenge/
// credential）、响应信封、鉴权边界（登记组须管理员 JWT、登录组匿名可达）与
// 错误码映射。完整 attestation/assertion 正负例在 app/admin/webauthn_test.go
//（软认证器驱动）——本文件不重复协议级用例，只钉 HTTP 契约。

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/contracts/errcode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	adminapp "github.com/pigeonbox/core/app/admin"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/auth"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/repo/db/model"
)

// newWebAuthnHTTPEnv 内存库 + 管理员（可选预登记一把 passkey）+ 四端点路由，
// 注入 handler 的包级服务与 RP 配置；Cleanup 双双还原。
func newWebAuthnHTTPEnv(t *testing.T, username string, enroll bool) (token string, credID string) {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, g.AutoMigrate(&model.User{}, &model.WebAuthnCredential{}))

	hash, err := bcrypt.GenerateFromPassword([]byte("admin-passw0rd!"), bcrypt.MinCost)
	require.NoError(t, err)
	u := &model.User{
		Username: username, Email: username + "@test.local",
		PasswordHash: string(hash), Role: "admin", Status: "active",
	}
	require.NoError(t, g.Create(u).Error)

	if enroll {
		raw := make([]byte, 32)
		_, err = rand.Read(raw)
		require.NoError(t, err)
		credID = base64.RawURLEncoding.EncodeToString(raw)
		require.NoError(t, g.Create(&model.WebAuthnCredential{
			UserID: u.ID, CredentialID: credID, PublicKey: []byte{0x58, 0x20, 0x01}, Name: "预登记",
		}).Error)
	}

	prevSvc := adminWebAuthnService
	adminWebAuthnService = adminapp.NewService(g)
	prevSnap := conf.Snapshot()
	conf.SetGlobalConfig(&conf.AppConfiguration{
		Security: conf.SecurityConfig{WebAuthn: conf.WebAuthnConfig{
			RPID: "share.example.com", RPOrigins: []string{"https://share.example.com"},
		}},
	})
	t.Cleanup(func() {
		adminWebAuthnService = prevSvc
		conf.SetGlobalConfig(prevSnap)
	})

	tok, err := auth.GenerateToken(u.ID, u.Username, "admin")
	require.NoError(t, err)
	return tok, credID
}

// postWebAuthn 打一次真实 POST（token 为空 = 匿名）。
func postWebAuthn(t *testing.T, h *server.Hertz, path, body, token string) (int, map[string]any) {
	t.Helper()
	req := ut.PerformRequest(h.Engine, http.MethodPost, path,
		&ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if token != "" {
		req = ut.PerformRequest(h.Engine, http.MethodPost, path,
			&ut.Body{Body: strings.NewReader(body), Len: len(body)},
			ut.Header{Key: "Content-Type", Value: "application/json"},
			ut.Header{Key: "Authorization", Value: "Bearer " + token})
	}
	var m map[string]any
	_ = json.Unmarshal(req.Body.Bytes(), &m)
	return req.Code, m
}

// publicKeyOf 从信封里取 data.publicKey（creation/assertion 共形）。
func publicKeyOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	data, ok := body["data"].(map[string]any)
	require.True(t, ok, "成功响应必须有 data, body=%v", body)
	pk, ok := data["publicKey"].(map[string]any)
	require.True(t, ok, "data.publicKey 必须在场, body=%v", body)
	return pk
}

func TestAdminWebAuthnEndpointsOverHTTP(t *testing.T) {
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.POST("/admin/webauthn/register/options", middleware.AdminMiddleware(), adminWebAuthnRegisterOptions)
	h.POST("/admin/webauthn/register/verify", middleware.AdminMiddleware(), adminWebAuthnRegisterVerify)
	h.POST("/admin/webauthn/login/options", adminWebAuthnLoginOptions)
	h.POST("/admin/webauthn/login/verify", adminWebAuthnLoginVerify)

	t.Run("①登记组必须管理员 JWT（匿名 401）", func(t *testing.T) {
		newWebAuthnHTTPEnv(t, "wa_http_nopass", false)
		st, body := postWebAuthn(t, h, "/admin/webauthn/register/options", `{}`, "")
		require.Equal(t, http.StatusUnauthorized, st, "匿名必须被挡, body=%v", body)
		require.EqualValues(t, errcode.CodeUnauthorized, body["code"])
	})

	t.Run("②登记 options 下发 challenge 与 RP", func(t *testing.T) {
		tok, _ := newWebAuthnHTTPEnv(t, "wa_http_reg", false)
		st, body := postWebAuthn(t, h, "/admin/webauthn/register/options", `{}`, tok)
		require.Equal(t, http.StatusOK, st, "body=%v", body)
		pk := publicKeyOf(t, body)
		assert.NotEmpty(t, pk["challenge"], "challenge 必须下发")
		rp, _ := pk["rp"].(map[string]any)
		require.NotNil(t, rp, "rp 必须下发, body=%v", body)
		assert.Equal(t, "share.example.com", rp["id"], "RP ID 必须取生效配置")
	})

	t.Run("③登记 verify 缺字段 ⇒ 400 且不落库", func(t *testing.T) {
		tok, _ := newWebAuthnHTTPEnv(t, "wa_http_regv", false)
		st, body := postWebAuthn(t, h, "/admin/webauthn/register/verify", `{"challenge":"x"}`, tok)
		require.Equal(t, http.StatusBadRequest, st, "body=%v", body)
		require.EqualValues(t, errcode.CodeInvalidParam, body["code"], "body=%v", body)
	})

	t.Run("④登录 options：已登记与未知名同形（防枚举）且带 allowCredentials", func(t *testing.T) {
		_, credID := newWebAuthnHTTPEnv(t, "wa_http_login", true)
		st, body := postWebAuthn(t, h, "/admin/webauthn/login/options", `{"username":"wa_http_login"}`, "")
		require.Equal(t, http.StatusOK, st, "登录挑战是公开面, body=%v", body)
		pk := publicKeyOf(t, body)
		allow, _ := pk["allowCredentials"].([]any)
		require.Len(t, allow, 1, "已登记必须下发 1 个凭据, body=%v", body)
		first, _ := allow[0].(map[string]any)
		assert.Equal(t, credID, first["id"], "凭据 ID 必须与落库一致")

		st2, body2 := postWebAuthn(t, h, "/admin/webauthn/login/options", `{"username":"wa_http_ghost"}`, "")
		require.Equal(t, http.StatusOK, st2, "诱饵必须同形返回, body=%v", body2)
		pk2 := publicKeyOf(t, body2)
		allow2, _ := pk2["allowCredentials"].([]any)
		require.Len(t, allow2, 1, "诱饵 allowCredentials 形状与真挑战一致")
		assert.NotEqual(t, pk["challenge"], pk2["challenge"], "两次 challenge 不得复用")
	})

	t.Run("⑤登录 options 缺 username ⇒ 400", func(t *testing.T) {
		newWebAuthnHTTPEnv(t, "wa_http_nouser", false)
		st, body := postWebAuthn(t, h, "/admin/webauthn/login/options", `{}`, "")
		require.Equal(t, http.StatusBadRequest, st, "body=%v", body)
		require.EqualValues(t, errcode.CodeInvalidParam, body["code"])
	})

	t.Run("⑥登录 verify 未知 challenge ⇒ 400 且不发 token", func(t *testing.T) {
		newWebAuthnHTTPEnv(t, "wa_http_verify", true)
		st, body := postWebAuthn(t, h, "/admin/webauthn/login/verify",
			`{"challenge":"no-such-challenge","credential":{"id":"x"}}`, "")
		require.Equal(t, http.StatusBadRequest, st, "body=%v", body)
		require.EqualValues(t, errcode.CodeInvalidParam, body["code"], "body=%v", body)
		require.NotContains(t, body, "data", "失败不得携带 token, body=%v", body)
	})
}
