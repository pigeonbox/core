package admin

// core#160 Passkey 闸的 wire 级负对照：contracts 冻结下 ErrWebAuthnRequired
// 复用 40012 流程信号（不计锁定、不发 token），且口令错不得泄露闸状态。
// service 层五条语义在 app/admin/webauthn_test.go；本文件钉 handler 分流。

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	adminapp "github.com/pigeonbox/core/app/admin"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/repo/db/model"
)

// newWebAuthnGateHTTPEnv 内存库 + 管理员（可选预登记 passkey）+ 闸开关。
func newWebAuthnGateHTTPEnv(t *testing.T, username string, gateOn, enroll bool) (password string) {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, g.AutoMigrate(&model.User{}, &model.WebAuthnCredential{}))

	password = "admin-passw0rd!"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
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
		require.NoError(t, g.Create(&model.WebAuthnCredential{
			UserID:       u.ID,
			CredentialID: base64.RawURLEncoding.EncodeToString(raw),
			PublicKey:    []byte{0x58, 0x20, 0x01}, Name: "k",
		}).Error)
	}

	adminService = adminapp.NewService(g)
	prevSnap := conf.Snapshot()
	conf.SetGlobalConfig(&conf.AppConfiguration{
		Security: conf.SecurityConfig{RequireAdminWebAuthn: gateOn},
	})
	t.Cleanup(func() {
		adminService = nil
		conf.SetGlobalConfig(prevSnap)
	})
	return password
}

// TestAdminLogin_WebAuthnGateOverHTTP /admin/login Passkey 闸的 wire 判据。
func TestAdminLogin_WebAuthnGateOverHTTP(t *testing.T) {
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.POST("/admin/login", AdminLogin)

	t.Run("①开闸+已登记+纯口令 ⇒ 40012 文案点名 Passkey、不发 token、不计锁定", func(t *testing.T) {
		pw := newWebAuthnGateHTTPEnv(t, "wa_http_gate", true, true)
		// 40012 不计失败：连打 10 次后第 11 次必须仍是 40012 而非 429（若计入锁定，
		// 默认阈值 10/300s 会在第 11 次转 429 ⇒ 本用例转红）。
		for i := 0; i < 11; i++ {
			st, body := postLogin(t, h, loginBody("wa_http_gate", pw, ""))
			require.Equal(t, http.StatusBadRequest, st, "第 %d 次: body=%v", i+1, body)
			require.EqualValues(t, 40012, body["code"], "第 %d 次: body=%v", i+1, body)
			msg, _ := body["message"].(string)
			require.Contains(t, msg, "Passkey", "文案必须点名 Passkey 供前端分流, body=%v", body)
			require.NotContains(t, body, "data", "流程信号不得携带 token, body=%v", body)
		}
	})

	t.Run("②开闸+未登记 ⇒ 纯口令照常放行（开闸不锁死存量）", func(t *testing.T) {
		pw := newWebAuthnGateHTTPEnv(t, "wa_http_gate_nocred", true, false)
		st, body := postLogin(t, h, loginBody("wa_http_gate_nocred", pw, ""))
		require.Equal(t, http.StatusOK, st, "body=%v", body)
		data, _ := body["data"].(map[string]any)
		assert.NotEmpty(t, data["token"], "必须发 token, body=%v", body)
	})

	t.Run("③开闸+已登记+口令错 ⇒ 凭据错误（不泄露闸状态）", func(t *testing.T) {
		_ = newWebAuthnGateHTTPEnv(t, "wa_http_gate_badpw", true, true)
		st, body := postLogin(t, h, loginBody("wa_http_gate_badpw", "wrong-password", ""))
		require.NotEqual(t, http.StatusOK, st, "口令错必须拒, body=%v", body)
		require.NotEqualValues(t, 40012, body["code"], "口令错不许回流程信号（否则可探测 passkey 登记状态）, body=%v", body)
		require.NotContains(t, body, "data")
	})

	t.Run("④默认关 ⇒ 已登记的纯口令照常放行（存量语义不变）", func(t *testing.T) {
		pw := newWebAuthnGateHTTPEnv(t, "wa_http_gate_off", false, true)
		st, body := postLogin(t, h, loginBody("wa_http_gate_off", pw, ""))
		require.Equal(t, http.StatusOK, st, "默认关必须与改造前一致, body=%v", body)
		data, _ := body["data"].(map[string]any)
		assert.NotEmpty(t, data["token"], "body=%v", body)
	})
}
