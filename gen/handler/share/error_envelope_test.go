// 错误信封统一（core#114）：gen/handler/share 的错误响应此前是 legacy map
// （无 trace_id），500 类把 err.Error() 原文出网（"获取分享列表失败: " + SQL 文本）。
// 判据同 request/notify：500 类脱敏 + 全站信封；业务/参数类保留文案与状态。
//
// 负对照（已实跑）：把 UserSharesList 的内部错误响应改回
// c.JSON(500, map{..., "message": "获取分享列表失败: " + err.Error()}) ⇒ 转红。
package share

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/contracts/errcode"
	shareService "github.com/pigeonbox/core/app/share"
)

// newBrokenShareEnv service 绑到**未建表**的内存库 ⇒ 查询稳定失败（内部错误路径）。
func newBrokenShareEnv(t *testing.T) {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	SetShareService(shareService.NewService(g, "http://test.local", nil))
	t.Cleanup(func() { SetShareService(nil) })
}

// doShareErrReq 以登录身份打一次用户分享管理面请求。
func doShareErrReq(t *testing.T, method, path, body string, userID uint) (int, map[string]any) {
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
	h.GET("/api/v1/user/shares", withIdentity(UserSharesList))
	h.POST("/api/v1/user/shares/batch-delete", withIdentity(UserSharesBatchDelete))

	w := ut.PerformRequest(h.Engine, method, path,
		&ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func shareTopKeys(body map[string]any) []string {
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	return keys
}

// assertShareErrEnvelope 全站信封判据（结构性：键集合/trace_id/无内部串）。
func assertShareErrEnvelope(t *testing.T, body map[string]any, wantCode int, internalMarkers ...string) {
	t.Helper()
	assert.Equal(t, float64(wantCode), body["code"], "业务码：%v", body)
	assert.NotEmpty(t, body["trace_id"], "错误响应必须带 trace_id 可对账：%v", body)
	assert.NotEmpty(t, body["message"], "%v", body)
	msg, _ := body["message"].(string)
	for _, m := range internalMarkers {
		assert.NotContains(t, msg, m, "内部错误细节不得出网：%v", body)
	}
	assert.ElementsMatch(t, []string{"code", "message", "trace_id"}, shareTopKeys(body),
		"错误响应应是全站信封：%v", body)
}

// TestShareErrorEnvelope_InternalSanitized 500 类（issue 点名的 1149/1198 形态）：
// 不进 HTTP 200、细节不出网。
func TestShareErrorEnvelope_InternalSanitized(t *testing.T) {
	newBrokenShareEnv(t)

	code, body := doShareErrReq(t, "GET", "/api/v1/user/shares", "", 7)
	assert.Equal(t, http.StatusInternalServerError, code,
		"UserSharesList 内部故障应 500（不得进 HTTP 200）：%v", body)
	assertShareErrEnvelope(t, body, errcode.CodeInternal, "no such table", "SELECT", "sql:")

	code, body = doShareErrReq(t, "POST", "/api/v1/user/shares/batch-delete", "{\"codes\":[\"ABCDEFGH\"]}", 7)
	assert.Equal(t, http.StatusInternalServerError, code,
		"UserSharesBatchDelete 内部故障应 500：%v", body)
	assertShareErrEnvelope(t, body, errcode.CodeInternal, "no such table", "SELECT", "sql:")
}

// TestShareErrorEnvelope_Param400 参数类：400 + 10001 + trace_id。
func TestShareErrorEnvelope_Param400(t *testing.T) {
	newBrokenShareEnv(t)

	code, body := doShareErrReq(t, "GET", "/api/v1/user/shares?page=abc", "", 7)
	assert.Equal(t, http.StatusBadRequest, code, "%v", body)
	assertShareErrEnvelope(t, body, errcode.CodeInvalidParam)
}
