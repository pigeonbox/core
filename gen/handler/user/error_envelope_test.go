// core#114 A6b：gen/handler/user 错误出口收编的 wire 判据。
//
// 判据：
//   - 内部（DB）故障 ⇒ 500 + 10008 + 全站信封（trace_id）+ 内部细节不出网；
//   - 业务可读错误（app/user 的 OpError）保留业务码与可操作文案，不被脱敏文案覆盖；
//   - 登录凭据错误保持 401 语义与文案（不是内部错误）。
//
// 负对照（已实跑）：把 UserStats 的错误分支回退为 legacy map + err.Error()
// ⇒ TestUserErrorEnvelope_InternalSanitized 转红。
package user

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/contracts/errcode"
	userApp "github.com/pigeonbox/core/app/user"
	"github.com/pigeonbox/core/pkg/resp"
)

// newBrokenUserEnv 绑到**未建表**的内存库 ⇒ 服务调用稳定失败（内部错误路径）。
func newBrokenUserEnv(t *testing.T) {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	SetUserService(userApp.NewService(g))
	t.Cleanup(func() { SetUserService(nil) })
}

// assertUserErrEnvelope 全站信封结构判据（键集恰为 {code,message,trace_id}）。
func assertUserErrEnvelope(t *testing.T, body map[string]any, wantCode int, internalMarkers ...string) {
	t.Helper()
	assert.Equal(t, float64(wantCode), body["code"], "业务码：%v", body)
	assert.NotEmpty(t, body["trace_id"], "错误响应必须带 trace_id 可对账：%v", body)
	msg, _ := body["message"].(string)
	assert.NotEmpty(t, msg, "%v", body)
	for _, m := range internalMarkers {
		assert.NotContains(t, msg, m, "内部错误细节不得出网：%v", body)
	}
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t, []string{"code", "message", "trace_id"}, keys,
		"错误响应应是全站信封：%v", body)
}

func TestUserErrorEnvelope_InternalSanitized(t *testing.T) {
	newBrokenUserEnv(t)
	for _, tc := range []struct{ method, path, body, what string }{
		{"GET", "/user/stats", "", "UserStats"},
		{"GET", "/user/files", `{"page":1,"page_size":20}`, "UserFiles"},
		{"GET", "/user/api-keys", "", "ListAPIKeys"},
		{"POST", "/user/api-keys/revoke-all", "", "RevokeAllAPIKeys"},
		{"PUT", "/user/profile", `{"nickname":"x","avatar":"a.png"}`, "UpdateProfile"},
	} {
		code, body := doUserReq(t, tc.method, tc.path, tc.body, 1)
		assert.Equal(t, http.StatusInternalServerError, code,
			"%s 坏库故障应 500（内部错误不得进 2xx）：%v", tc.what, body)
		assertUserErrEnvelope(t, body, errcode.CodeInternal, "no such table", "SELECT", "sql:")
	}
}

// TestUserErrorEnvelope_BusinessTextPreserved 业务可读错误保留文案与业务码
// （不得被脱敏成「操作失败，请稍后重试」——那会把「旧密码错误」这类可操作信息抹掉）。
func TestUserErrorEnvelope_BusinessTextPreserved(t *testing.T) {
	newBrokenUserEnv(t)
	// ChangePassword：坏库下 app/user 回业务可读错误（OpError，10001）⇒ 400 + 文案保留
	code, body := doUserReq(t, "POST", "/user/change-password",
		`{"old_password":"a","new_password":"bbbbbb"}`, 1)
	assert.Equal(t, http.StatusBadRequest, code, "%v", body)
	assertUserErrEnvelope(t, body, errcode.CodeInvalidParam)
	assert.NotEqual(t, resp.InternalFailureMsg("修改密码"), body["message"],
		"业务可读错误不得被脱敏文案覆盖：%v", body)
}

// doLoginReq 打 /user/login（登录面独立台架：不在 doUserReq 的路由清单里）。
func doLoginReq(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.POST("/user/login", Login)
	w := ut.PerformRequest(h.Engine, "POST", "/user/login",
		&ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// TestLoginErrorEnvelope_BadCredentials401_ DBFault500 凭据错误保持 401+可操作文案；
// DB 故障走内部脱敏（此前 catch-all 一律 401 + err.Error()——SQL 文本随 401 出网）。
func TestLoginErrorEnvelope_Credential401_DBFault500(t *testing.T) {
	// 凭据错误：真实库 + 错误口令
	svc := newUserHandlerEnv(t)
	_, err := svc.Create(context.Background(), &userApp.CreateUserReq{
		Username: "alice", Email: "alice@example.com", Password: "secret123",
	})
	require.NoError(t, err)
	code, body := doLoginReq(t, `{"username":"alice","password":"wrong"}`)
	assert.Equal(t, http.StatusUnauthorized, code,
		"凭据错误应 401（不是 500）：%v", body)
	assertUserErrEnvelope(t, body, errcode.CodeUnauthorized)
	assert.NotEqual(t, resp.InternalFailureMsg("登录"), body["message"],
		"凭据错误文案不得被脱敏覆盖：%v", body)

	// DB 故障：坏库 ⇒ 500 + 10008 + 脱敏
	newBrokenUserEnv(t)
	code, body = doLoginReq(t, `{"username":"alice","password":"wrong"}`)
	assert.Equal(t, http.StatusInternalServerError, code,
		"登录链路 DB 故障应 500（不得把 SQL 文本随 401 出网）：%v", body)
	assertUserErrEnvelope(t, body, errcode.CodeInternal, "no such table", "SELECT", "sql:")
}
