package bootstrap

// core#160 管理员 Passkey 四端点的 wire 级判据：字段绑定（username/challenge/
// credential）、响应信封、鉴权边界（登记组须管理员 JWT、登录组匿名可达）与
// 错误码映射。完整 attestation/assertion 正负例在 app/admin/webauthn_test.go
//（软认证器驱动）——本文件不重复协议级用例，只钉 HTTP 契约。

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

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

// webAuthnHTTPEnv 一套 webauthn HTTP 测试装置：内存库 + 管理员 + 令牌
// （可选预登记一把 passkey），并暴露 DB 供跨用户/字段形状用例追加数据。
type webAuthnHTTPEnv struct {
	g         *gorm.DB
	user      *model.User
	token     string
	credID    string
	credRowID uint
}

// newWebAuthnHTTPEnvFull 构造完整装置并注入 handler 的包级服务与 RP 配置；
// Cleanup 双双还原（同一用例内追加用户/凭证直接复用 env.g）。
func newWebAuthnHTTPEnvFull(t *testing.T, username string, enroll bool) *webAuthnHTTPEnv {
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

	env := &webAuthnHTTPEnv{g: g, user: u}
	if enroll {
		env.credID, env.credRowID = addWebAuthnHTTPCredential(t, g, u.ID, "预登记")
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
	env.token = tok
	return env
}

// newWebAuthnHTTPEnv 兼容旧用例的瘦返回值（令牌 + 预登记凭证 ID）。
func newWebAuthnHTTPEnv(t *testing.T, username string, enroll bool) (token string, credID string) {
	t.Helper()
	env := newWebAuthnHTTPEnvFull(t, username, enroll)
	return env.token, env.credID
}

// addWebAuthnHTTPUser 同一内存库里追加一个在职管理员，返回用户与管理员 JWT。
func addWebAuthnHTTPUser(t *testing.T, g *gorm.DB, username string) (*model.User, string) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("admin-passw0rd!"), bcrypt.MinCost)
	require.NoError(t, err)
	u := &model.User{
		Username: username, Email: username + "@test.local",
		PasswordHash: string(hash), Role: "admin", Status: "active",
	}
	require.NoError(t, g.Create(u).Error)
	tok, err := auth.GenerateToken(u.ID, u.Username, "admin")
	require.NoError(t, err)
	return u, tok
}

// addWebAuthnHTTPCredential 落一把随机凭证（公钥是占位字节——本文件只测
// HTTP 契约，真实 attestation/assertion 正负例在 app/admin/webauthn_test.go）。
func addWebAuthnHTTPCredential(t *testing.T, g *gorm.DB, userID uint, name string) (credID string, rowID uint) {
	t.Helper()
	raw := make([]byte, 32)
	_, err := rand.Read(raw)
	require.NoError(t, err)
	credID = base64.RawURLEncoding.EncodeToString(raw)
	row := &model.WebAuthnCredential{
		UserID: userID, CredentialID: credID, PublicKey: []byte{0x58, 0x20, 0x01}, Name: name,
	}
	require.NoError(t, g.Create(row).Error)
	return credID, row.ID
}

// webAuthnRequest 打一次真实请求（token 为空 = 匿名；body 为空 = 无请求体）。
func webAuthnRequest(t *testing.T, h *server.Hertz, method, path, body, token string) (int, map[string]any) {
	t.Helper()
	var reqBody *ut.Body
	headers := make([]ut.Header, 0, 2)
	if body != "" {
		reqBody = &ut.Body{Body: strings.NewReader(body), Len: len(body)}
		headers = append(headers, ut.Header{Key: "Content-Type", Value: "application/json"})
	}
	if token != "" {
		headers = append(headers, ut.Header{Key: "Authorization", Value: "Bearer " + token})
	}
	req := ut.PerformRequest(h.Engine, method, path, reqBody, headers...)
	var m map[string]any
	_ = json.Unmarshal(req.Body.Bytes(), &m)
	return req.Code, m
}

// postWebAuthn 打一次真实 POST（token 为空 = 匿名）。
func postWebAuthn(t *testing.T, h *server.Hertz, path, body, token string) (int, map[string]any) {
	t.Helper()
	return webAuthnRequest(t, h, http.MethodPost, path, body, token)
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

// TestAdminWebAuthnCredentialManagementOverHTTP core#209 管理台凭证管理
// list/delete 两端点的 wire 级判据：鉴权边界、字段形状（不泄露内部字段）、
// 归属隔离（跨用户 404）、删除幂等（重复删除稳定 404、硬删不留行）、
// 删除后登录面不再提供该凭证。
func TestAdminWebAuthnCredentialManagementOverHTTP(t *testing.T) {
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.GET("/admin/webauthn/credentials", middleware.AdminMiddleware(), adminWebAuthnCredentialList)
	h.DELETE("/admin/webauthn/credentials/:id", middleware.AdminMiddleware(), adminWebAuthnCredentialDelete)
	// ⑥ 需要登录面：删除后 options 不得再给出已删凭证
	h.POST("/admin/webauthn/login/options", adminWebAuthnLoginOptions)
	h.POST("/admin/webauthn/login/verify", adminWebAuthnLoginVerify)

	// listData 取成功信封的 data 数组（形状固定：本条契约的一部分）。
	listData := func(t *testing.T, body map[string]any) []any {
		t.Helper()
		data, ok := body["data"].([]any)
		require.True(t, ok, "list 成功响应 data 必须是数组, body=%v", body)
		return data
	}
	// allowIDs 取登录 options 下发的 allowCredentials id 列表。
	allowIDs := func(t *testing.T, body map[string]any) []any {
		t.Helper()
		pk := publicKeyOf(t, body)
		allow, _ := pk["allowCredentials"].([]any)
		require.NotEmpty(t, allow, "allowCredentials 必须非空（真挑战/诱饵同形）, body=%v", body)
		out := make([]any, 0, len(allow))
		for _, a := range allow {
			item, _ := a.(map[string]any)
			out = append(out, item["id"])
		}
		return out
	}

	t.Run("①匿名不可达（list/delete 均 401）", func(t *testing.T) {
		newWebAuthnHTTPEnvFull(t, "wa_cred_anon", false)
		st, body := webAuthnRequest(t, h, http.MethodGet, "/admin/webauthn/credentials", "", "")
		require.Equal(t, http.StatusUnauthorized, st, "body=%v", body)
		require.EqualValues(t, errcode.CodeUnauthorized, body["code"], "body=%v", body)

		st, body = webAuthnRequest(t, h, http.MethodDelete, "/admin/webauthn/credentials/1", "", "")
		require.Equal(t, http.StatusUnauthorized, st, "body=%v", body)
		require.EqualValues(t, errcode.CodeUnauthorized, body["code"], "body=%v", body)
	})

	t.Run("②list 只回自己 + 字段形状固定（不泄露内部字段）", func(t *testing.T) {
		env := newWebAuthnHTTPEnvFull(t, "wa_cred_list", true)
		used := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
		require.NoError(t, env.g.Model(&model.WebAuthnCredential{}).Where("id = ?", env.credRowID).
			Updates(map[string]any{
				"transports":   "usb,internal",
				"aa_guid":      "00112233445566778899aabbccddeeff",
				"last_used_at": &used,
			}).Error)

		st, body := webAuthnRequest(t, h, http.MethodGet, "/admin/webauthn/credentials", "", env.token)
		require.Equal(t, http.StatusOK, st, "body=%v", body)
		items := listData(t, body)
		require.Len(t, items, 1, "只回当前管理员的凭证, body=%v", body)
		item, ok := items[0].(map[string]any)
		require.True(t, ok, "list 条目必须是对象, body=%v", body)

		keys := make([]string, 0, len(item))
		for k := range item {
			keys = append(keys, k)
		}
		assert.ElementsMatch(t,
			[]string{"id", "name", "created_at", "last_used_at", "transports", "aaguid"},
			keys, "凭证列表字段形状是 wire 契约, item=%v", item)
		for _, forbidden := range []string{"credential_id", "public_key", "user_id", "sign_count", "updated_at", "deleted_at"} {
			assert.NotContains(t, item, forbidden, "内部/敏感字段不得出网: %s", forbidden)
		}

		assert.EqualValues(t, env.credRowID, item["id"])
		assert.Equal(t, "预登记", item["name"])
		assert.NotEmpty(t, item["created_at"], "created_at 必须在场")
		assert.NotEmpty(t, item["last_used_at"], "last_used_at 必须在场（未使用可为 null）")
		assert.Equal(t, []any{"usb", "internal"}, item["transports"], "transports 必须是字符串数组")
		assert.Equal(t, "00112233445566778899aabbccddeeff", item["aaguid"])

		// 跨用户隔离：另一个管理员只看到自己的两把
		other, otherTok := addWebAuthnHTTPUser(t, env.g, "wa_cred_list_other")
		addWebAuthnHTTPCredential(t, env.g, other.ID, "他人的一")
		_, _ = addWebAuthnHTTPCredential(t, env.g, other.ID, "他人的二")

		st, body = webAuthnRequest(t, h, http.MethodGet, "/admin/webauthn/credentials", "", otherTok)
		require.Equal(t, http.StatusOK, st, "body=%v", body)
		otherItems := listData(t, body)
		require.Len(t, otherItems, 2, "他人只看到自己的凭证, body=%v", body)
		for _, raw := range otherItems {
			it, _ := raw.(map[string]any)
			assert.NotEqualValues(t, env.credRowID, it["id"], "不得混入他人凭证")
			assert.NotEqual(t, "预登记", it["name"])
		}
	})

	t.Run("③跨用户 delete ⇒ 404（不泄露存在性）且行保留", func(t *testing.T) {
		env := newWebAuthnHTTPEnvFull(t, "wa_cred_cross", true)
		_, otherTok := addWebAuthnHTTPUser(t, env.g, "wa_cred_cross_other")

		st, body := webAuthnRequest(t, h, http.MethodDelete,
			fmt.Sprintf("/admin/webauthn/credentials/%d", env.credRowID), "", otherTok)
		require.Equal(t, http.StatusNotFound, st, "跨用户删除必须 404, body=%v", body)
		require.EqualValues(t, errcode.CodeNotFound, body["code"], "body=%v", body)

		// 行未被删除：所有者仍可见、DB 计数仍为 1
		st, body = webAuthnRequest(t, h, http.MethodGet, "/admin/webauthn/credentials", "", env.token)
		require.Equal(t, http.StatusOK, st, "body=%v", body)
		require.Len(t, listData(t, body), 1, "跨用户删除不得影响他人凭证, body=%v", body)
		var n int64
		require.NoError(t, env.g.Model(&model.WebAuthnCredential{}).
			Where("user_id = ? AND id = ?", env.user.ID, env.credRowID).Count(&n).Error)
		assert.EqualValues(t, 1, n, "凭证行必须保留")
	})

	t.Run("④delete 幂等：首次 200、重复 404、硬删不留行", func(t *testing.T) {
		env := newWebAuthnHTTPEnvFull(t, "wa_cred_del", true)

		path := fmt.Sprintf("/admin/webauthn/credentials/%d", env.credRowID)
		st, body := webAuthnRequest(t, h, http.MethodDelete, path, "", env.token)
		require.Equal(t, http.StatusOK, st, "body=%v", body)
		require.EqualValues(t, 0, body["code"], "body=%v", body)

		// 硬删：行彻底消失（软删会占住 credential_id 唯一索引，同一张
		// passkey 撤销后无法重新登记）
		var n int64
		require.NoError(t, env.g.Unscoped().Model(&model.WebAuthnCredential{}).
			Where("id = ?", env.credRowID).Count(&n).Error)
		assert.Zero(t, n, "删除必须是硬删")

		// 重复删除：效果幂等（行保持不存在），响应稳定 404
		st, body = webAuthnRequest(t, h, http.MethodDelete, path, "", env.token)
		require.Equal(t, http.StatusNotFound, st, "重复删除必须稳定 404, body=%v", body)
		require.EqualValues(t, errcode.CodeNotFound, body["code"], "body=%v", body)

		st, body = webAuthnRequest(t, h, http.MethodGet, "/admin/webauthn/credentials", "", env.token)
		require.Equal(t, http.StatusOK, st, "body=%v", body)
		assert.Empty(t, listData(t, body), "删除后列表为空, body=%v", body)
	})

	t.Run("⑤非法 id ⇒ 400", func(t *testing.T) {
		env := newWebAuthnHTTPEnvFull(t, "wa_cred_badid", false)
		for _, id := range []string{"abc", "0", "-1", "1.5"} {
			st, body := webAuthnRequest(t, h, http.MethodDelete,
				"/admin/webauthn/credentials/"+id, "", env.token)
			require.Equal(t, http.StatusBadRequest, st, "id=%q body=%v", id, body)
			require.EqualValues(t, errcode.CodeInvalidParam, body["code"], "id=%q body=%v", id, body)
		}
	})

	t.Run("⑥删除后登录面不再提供该凭证且验证被拒", func(t *testing.T) {
		env := newWebAuthnHTTPEnvFull(t, "wa_cred_login", true)

		// 删除前：登录 options 必须下发这把凭证
		st, body := postWebAuthn(t, h, "/admin/webauthn/login/options",
			`{"username":"wa_cred_login"}`, "")
		require.Equal(t, http.StatusOK, st, "body=%v", body)
		ids := allowIDs(t, body)
		require.Len(t, ids, 1, "删除前必须只下发这一把, body=%v", body)
		assert.Equal(t, env.credID, ids[0])

		// 撤销
		st, body = webAuthnRequest(t, h, http.MethodDelete,
			fmt.Sprintf("/admin/webauthn/credentials/%d", env.credRowID), "", env.token)
		require.Equal(t, http.StatusOK, st, "body=%v", body)

		// 删除后：options 回诱饵形态（同形但不再含已删凭证），验证必然被拒
		st, body = postWebAuthn(t, h, "/admin/webauthn/login/options",
			`{"username":"wa_cred_login"}`, "")
		require.Equal(t, http.StatusOK, st, "诱饵必须同形返回, body=%v", body)
		ids = allowIDs(t, body)
		require.Len(t, ids, 1, "诱饵形状与真挑战一致, body=%v", body)
		assert.NotEqual(t, env.credID, ids[0], "已删除凭证不得再出现在 allowCredentials")

		pk := publicKeyOf(t, body)
		challenge, _ := pk["challenge"].(string)
		require.NotEmpty(t, challenge)
		st, verifyBody := postWebAuthn(t, h, "/admin/webauthn/login/verify",
			fmt.Sprintf(`{"challenge":%q,"credential":{"id":%q,"rawId":%q,"type":"public-key","response":{}}}`,
				challenge, env.credID, env.credID), "")
		require.Equal(t, http.StatusBadRequest, st, "删除后登录必须被拒, body=%v", verifyBody)
		require.EqualValues(t, errcode.CodeInvalidParam, verifyBody["code"], "body=%v", verifyBody)
		require.NotContains(t, verifyBody, "data", "失败不得签发 token, body=%v", verifyBody)
	})
}
