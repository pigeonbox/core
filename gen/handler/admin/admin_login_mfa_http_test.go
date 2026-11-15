// core#162 残差（#177 合并后的 HTTP 级负对照）：/admin/login 二验的端到端
// wire 判据。#177 在 service 层钉了五条语义（app/admin/auth_totp_test.go）、
// handler 里做了 40012/40013 分流，但**契约字段绑定（totp_code）→ errcode
// → HTTP 状态 → 「不得发 token」**这条链只有走真实 handler 才验得到。
//
// issue #162 修复方向 4 的负对照（逐字）：启用 TOTP 的 admin 用正确口令 +
// 无码调 /admin/login ⇒ 必须 40012，不得发 token。
//
// 额外判别（锁定语义，阈值=默认 10/300s）：
//   - 连续 10 次 40012 后正确码登录必须仍 200 ⇒ 40012 绝不计失败
//     （若被改成 RecordFailure，第 10 次即锁 ⇒ 转 429 红）；
//   - 连续 10 次 40013 后第 11 次必须被锁 ⇒ 40013 计入锁定桶
//     （二验码可无限试的回归面）。
package admin

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

	adminapp "github.com/pigeonbox/core/app/admin"
	"github.com/pigeonbox/core/pkg/totp"
	"github.com/pigeonbox/core/repo/db/model"
)

// newAdminLoginEnv 建内存库、种指定用户名的管理员，注入 AdminLogin 的包级
// adminService（同包直注；Cleanup 归零，避免污染其他用例的惰性 Default）。
func newAdminLoginEnv(t *testing.T, username string, totpEnabled bool) (password, secret string) {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, g.AutoMigrate(&model.User{}))

	password = "admin-passw0rd!"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	require.NoError(t, err)
	secret, err = totp.GenerateSecret()
	require.NoError(t, err)
	require.NoError(t, g.Create(&model.User{
		Username: username, Email: username + "@test.local",
		PasswordHash: string(hash), Role: "admin", Status: "active",
		TotpEnabled: totpEnabled, TotpSecret: secret,
	}).Error)

	adminService = adminapp.NewService(g)
	t.Cleanup(func() { adminService = nil })
	return password, secret
}

// postLogin 打一次真实 POST /admin/login，返回 (HTTP 状态, 解析后的 body)。
func postLogin(t *testing.T, h *server.Hertz, body string) (int, map[string]any) {
	t.Helper()
	w := ut.PerformRequest(h.Engine, http.MethodPost, "/admin/login",
		&ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	var m map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	return w.Code, m
}

func loginBody(username, password, totpCode string) string {
	b := map[string]string{"username": username, "password": password}
	if totpCode != "" {
		b["totp_code"] = totpCode
	}
	raw, _ := json.Marshal(b)
	return string(raw)
}

// TestAdminLogin_TOTPOverHTTP /admin/login 二验的 wire 级判据（core#162）。
func TestAdminLogin_TOTPOverHTTP(t *testing.T) {
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.POST("/admin/login", AdminLogin)

	t.Run("①正确口令+无码 ⇒ 40012 不发 token；且 40012 不计锁定", func(t *testing.T) {
		pw, secret := newAdminLoginEnv(t, "mfa_missing", true)
		code, err := totp.Code(secret, time.Now())
		require.NoError(t, err)

		// 负对照本体：正确口令 + 无码 ⇒ 40012，不得发 token
		for i := 0; i < 10; i++ {
			st, body := postLogin(t, h, loginBody("mfa_missing", pw, ""))
			require.Equal(t, http.StatusBadRequest, st, "缺码必须 400, got %d body=%v", st, body)
			require.EqualValues(t, 40012, body["code"], "缺码必须 40012（流程信号）, body=%v", body)
			require.NotContains(t, body, "data", "40012 不得携带 token/会话, body=%v", body)
		}

		// 40012 不计失败：默认阈值 10/300s——若缺码被计入，第 10 次即锁，
		// 下面这次正确码登录会被 429 拦下（转红）。
		st, body := postLogin(t, h, loginBody("mfa_missing", pw, code))
		require.Equal(t, http.StatusOK, st, "40012 不计锁定，正确码登录必须放行, body=%v", body)
		require.EqualValues(t, 0, body["code"], "成功必须 code=0, body=%v", body)
		data, _ := body["data"].(map[string]any)
		require.NotEmpty(t, data["token"], "成功必须发 token, body=%v", body)
	})

	t.Run("②错码 ⇒ 40013 且计入锁定桶", func(t *testing.T) {
		pw, _ := newAdminLoginEnv(t, "mfa_wrong", true)

		for i := 0; i < 10; i++ {
			st, body := postLogin(t, h, loginBody("mfa_wrong", pw, "000000"))
			require.Equal(t, http.StatusBadRequest, st, "错码必须 400, got %d body=%v", st, body)
			require.EqualValues(t, 40013, body["code"], "错码必须 40013, body=%v", body)
		}
		// 40013 计失败：第 11 次应在锁定检查处被拦（否则二验码可无限试）
		st, body := postLogin(t, h, loginBody("mfa_wrong", pw, "000000"))
		require.NotEqual(t, http.StatusOK, st, "40013 计入锁定，阈值后必须被拦, body=%v", body)
		msg, _ := body["message"].(string)
		require.Contains(t, msg, "锁定", "锁定拦截必须有可判文案, body=%v", body)
	})

	t.Run("③正确码 ⇒ 200 发 token（契约 totp_code 字段绑定）", func(t *testing.T) {
		pw, secret := newAdminLoginEnv(t, "mfa_ok", true)
		code, err := totp.Code(secret, time.Now())
		require.NoError(t, err)

		st, body := postLogin(t, h, loginBody("mfa_ok", pw, code))
		require.Equal(t, http.StatusOK, st, "正确码必须放行, body=%v", body)
		require.EqualValues(t, 0, body["code"], "成功必须 code=0, body=%v", body)
		data, _ := body["data"].(map[string]any)
		assert.NotEmpty(t, data["token"], "成功必须发 token")
	})
}
