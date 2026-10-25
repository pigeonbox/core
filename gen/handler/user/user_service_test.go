// user HTTP 面的**边界**断言（2026-10-25 补零测试面：
// gen/handler/user 此前 0% 覆盖 ⇒ 「登录专属」与「归属校验」无人守护）。
//
// 该域的面几乎全是**登录专属**（/user/info、/user/stats、/user/files、
// /user/profile、/user/change-password、/user/api-keys*）⇒ 越权/泄漏是头号风险：
//
//	· 未登录访问 ⇒ 401（**不得**返回 200 + 空 data 冒充成功）；
//	· API Key 吊销 ⇒ **归属校验**（A 用户不得吊销 B 用户的密钥）；
//	· 一键吊销 ⇒ 只影响**自己**的密钥（不得误伤他人）。
//
// 断言必须能变红（负对照）：
//
//	· 把 DeleteAPIKey 的 userID 传错 ⇒ 归属用例红；
//	· 把 UserInfo 的 401 改回 200 ⇒ 未登录用例红。
package user

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	userApp "github.com/pigeonbox/core/app/user"
	"github.com/pigeonbox/core/repo/db/model"
)

// newUserHandlerEnv 构造 user handler 环境（内存 sqlite + 域服务）。
func newUserHandlerEnv(t *testing.T) *userApp.Service {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.User{}, &model.UserAPIKey{}, &model.FileCode{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	svc := userApp.NewService(g)
	SetUserService(svc)
	t.Cleanup(func() { SetUserService(nil) })
	return svc
}

// doUserReq 以指定身份调一个 handler，返回 HTTP 码与响应体。
// userID=0 ⇒ 匿名（不写 user_id ⇒ 模拟「未登录」）。
func doUserReq(t *testing.T, method, path, body string, userID uint) (int, map[string]any) {
	t.Helper()
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	withIdentity := func(handler app.HandlerFunc) app.HandlerFunc {
		return func(ctx context.Context, c *app.RequestContext) {
			if userID != 0 {
				c.Set("user_id", userID)
			}
			handler(ctx, c)
		}
	}
	h.GET("/user/info", withIdentity(UserInfo))
	h.GET("/user/stats", withIdentity(UserStats))
	h.GET("/user/files", withIdentity(UserFiles))
	h.PUT("/user/profile", withIdentity(UpdateProfile))
	h.POST("/user/change-password", withIdentity(ChangePassword))
	h.GET("/user/api-keys", withIdentity(ListAPIKeys))
	h.POST("/user/api-keys", withIdentity(CreateAPIKey))
	h.DELETE("/user/api-keys/:id", withIdentity(DeleteAPIKey))
	h.POST("/user/api-keys/revoke-all", withIdentity(RevokeAllAPIKeys))

	w := ut.PerformRequest(h.Engine, method, path,
		&ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// TestLoginOnlyFaces_RequireLogin 全部**登录专属**面：未登录 ⇒ 401。
//
// 负对照：把 UserInfo 的未登录分支改回 200 ⇒ 本用例红（该面返回 200）。
func TestLoginOnlyFaces_RequireLogin(t *testing.T) {
	newUserHandlerEnv(t)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"user/info", "GET", "/user/info", ""},
		{"user/stats", "GET", "/user/stats", ""},
		{"user/files", "GET", "/user/files", ""},
		{"user/profile", "PUT", "/user/profile", `{"nickname":"x"}`},
		{"user/change-password", "POST", "/user/change-password", `{"old_password":"a","new_password":"b"}`},
		{"user/api-keys 列表", "GET", "/user/api-keys", ""},
		{"user/api-keys 新建", "POST", "/user/api-keys", `{"name":"k"}`},
		{"user/api-keys 吊销", "DELETE", "/user/api-keys/1", ""},
		{"user/api-keys 一键吊销", "POST", "/user/api-keys/revoke-all", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := doUserReq(t, c.method, c.path, c.body, 0)
			assert.Equal(t, http.StatusUnauthorized, code,
				"未登录访问 %s 应 401（不得 200 冒充成功）：%v", c.path, body)
		})
	}
}

// TestDeleteAPIKey_RejectsNonOwner 吊销**别人**的 API Key 必须失败（归属校验）。
//
// 负对照：handler 把 userID 传成 0/固定值，或 dao 的 RevokeByID 去掉
// `user_id = ?` 条件 ⇒ 本用例红。
func TestDeleteAPIKey_RejectsNonOwner(t *testing.T) {
	svc := newUserHandlerEnv(t)
	ctx := context.Background()

	// user 1 与 user 2 各建一把密钥
	ownerKey, err := svc.CreateAPIKey(ctx, 1, &userApp.CreateAPIKeyReq{Name: "owner"})
	require.NoError(t, err)
	otherKey, err := svc.CreateAPIKey(ctx, 2, &userApp.CreateAPIKeyReq{Name: "other"})
	require.NoError(t, err)

	// user 2 试图吊销 user 1 的密钥 ⇒ 必须失败
	code, body := doUserReq(t, "DELETE", "/user/api-keys/"+strconv.Itoa(int(ownerKey.APIKey.ID)), "", 2)
	assert.NotEqual(t, http.StatusOK, code,
		"非归属用户吊销别人的 API Key 必须被拒：%v", body)

	// 且那把密钥**不得**被标记为已撤销
	still, err := svc.ListAPIKeys(ctx, 1)
	require.NoError(t, err)
	require.Len(t, still, 1)
	assert.False(t, still[0].Revoked, "被拒绝的吊销不得改到他人密钥：%v", still[0])

	// 反向对照：属主自己吊销 ⇒ 成功
	code, body = doUserReq(t, "DELETE", "/user/api-keys/"+strconv.Itoa(int(otherKey.APIKey.ID)), "", 2)
	assert.Equal(t, http.StatusOK, code, "属主吊销自己的密钥应成功：%v", body)
}

// TestRevokeAllAPIKeys_OnlyAffectsOwn 一键吊销**只影响自己**的密钥。
//
// 负对照：dao 的 RevokeAllByUser 去掉 `user_id = ?` 条件 ⇒ 本用例红。
func TestRevokeAllAPIKeys_OnlyAffectsOwn(t *testing.T) {
	svc := newUserHandlerEnv(t)
	ctx := context.Background()

	_, err := svc.CreateAPIKey(ctx, 1, &userApp.CreateAPIKeyReq{Name: "a"})
	require.NoError(t, err)
	_, err = svc.CreateAPIKey(ctx, 1, &userApp.CreateAPIKeyReq{Name: "b"})
	require.NoError(t, err)
	_, err = svc.CreateAPIKey(ctx, 2, &userApp.CreateAPIKeyReq{Name: "c"})
	require.NoError(t, err)

	code, body := doUserReq(t, "POST", "/user/api-keys/revoke-all", "", 1)
	require.Equal(t, http.StatusOK, code, "一键吊销应 200：%v", body)

	mine, err := svc.ListAPIKeys(ctx, 1)
	require.NoError(t, err)
	require.Len(t, mine, 2)
	for _, k := range mine {
		assert.True(t, k.Revoked, "user1 自己的密钥应被吊销：%v", k)
	}

	other, err := svc.ListAPIKeys(ctx, 2)
	require.NoError(t, err)
	require.Len(t, other, 1)
	assert.False(t, other[0].Revoked, "一键吊销**不得**误伤他人密钥：%v", other[0])
}

// TestListAPIKeys_OnlyOwn 列表**只含自己的**密钥（不泄露他人密钥的 id/前缀）。
//
// 负对照：dao 的 ListByUser 去掉 `user_id = ?` 条件 ⇒ 本用例红。
func TestListAPIKeys_OnlyOwn(t *testing.T) {
	svc := newUserHandlerEnv(t)
	ctx := context.Background()

	_, err := svc.CreateAPIKey(ctx, 1, &userApp.CreateAPIKeyReq{Name: "mine"})
	require.NoError(t, err)
	_, err = svc.CreateAPIKey(ctx, 2, &userApp.CreateAPIKeyReq{Name: "theirs"})
	require.NoError(t, err)

	code, body := doUserReq(t, "GET", "/user/api-keys", "", 1)
	require.Equal(t, http.StatusOK, code, "列表应 200：%v", body)

	keys, _ := body["keys"].([]any)
	require.Len(t, keys, 1, "列表只应含自己的密钥：%v", body)
	first, _ := keys[0].(map[string]any)
	assert.Equal(t, "mine", first["name"])
}
