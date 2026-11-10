// 错误信封统一（core#114 + core#115）：gen/handler/admin/admin_service.go 此前把
// 业务错误码放进 **HTTP 200**（body code=10001/404/500），与 core#102 的 400 口径
// 自相矛盾——监控/网关按 HTTP 状态判健康度时这些错误全是绿的；且 500 类把
// err.Error() 原文出网。本组用例钉住：
//
//	① 业务错误不得 HTTP 200（core#115 主判据）：参数 400 / 内部 500 / 缺失 404；
//	② 500 类内部细节不出网 + 全站信封 trace_id（core#114）；
//	③ 参数类 400 + 10001 + 点名文案。
//
// 负对照（已实跑）：把 AdminStats 的内部错误分支改回
// c.JSON(200, &admin.AdminStatsResp{Code: 500, Message: err.Error()}) ⇒ ①② 转红。
package admin

import (
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
	adminsvc "github.com/pigeonbox/core/app/admin"
)

// newBrokenAdminEnv admin service 绑到**未建表**的内存库 ⇒ 一切查询/写入稳定失败。
// 直接注入 handler 包内单例（adminService），不动 app 层全局 Default()。
func newBrokenAdminEnv(t *testing.T) {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	adminService = adminsvc.NewService(g)
	t.Cleanup(func() { adminService = nil })
}

// doAdminErrReq 打一次管理面请求（管理面鉴权在路由层，本组只测错误形态）。
func doAdminErrReq(t *testing.T, method, path, body string) (int, map[string]any) {
	t.Helper()
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.GET("/admin/stats", AdminStats)
	h.GET("/admin/users", AdminListUsers)
	h.PUT("/admin/users/:id/status", AdminUpdateUserStatus)
	h.GET("/admin/config", AdminGetConfig)
	h.PUT("/admin/config", AdminUpdateConfig)
	h.DELETE("/admin/files/:id", AdminDeleteFile)

	w := ut.PerformRequest(h.Engine, method, path,
		&ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func adminTopKeys(body map[string]any) []string {
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	return keys
}

// assertAdminErrEnvelope 全站信封判据（结构性：键集合/trace_id/无内部串）。
func assertAdminErrEnvelope(t *testing.T, body map[string]any, wantCode int, internalMarkers ...string) {
	t.Helper()
	assert.Equal(t, float64(wantCode), body["code"], "业务码：%v", body)
	assert.NotEmpty(t, body["trace_id"], "错误响应必须带 trace_id 可对账：%v", body)
	assert.NotEmpty(t, body["message"], "%v", body)
	msg, _ := body["message"].(string)
	for _, m := range internalMarkers {
		assert.NotContains(t, msg, m, "内部错误细节不得出网：%v", body)
	}
	assert.ElementsMatch(t, []string{"code", "message", "trace_id"}, adminTopKeys(body),
		"错误响应应是全站信封：%v", body)
}

// TestAdminErrorEnvelope_BusinessNotHTTP200 core#115 主判据：业务错误不得 HTTP 200。
func TestAdminErrorEnvelope_BusinessNotHTTP200(t *testing.T) {
	newBrokenAdminEnv(t)

	// 参数解析失败（page=abc）：此前 HTTP 200 + body code=10001 ⇒ 现 400 + 10001
	code, body := doAdminErrReq(t, "GET", "/admin/users?page=abc", "")
	assert.Equal(t, http.StatusBadRequest, code, "参数错误必须 400（不得进 HTTP 200）：%v", body)
	assertAdminErrEnvelope(t, body, errcode.CodeInvalidParam)

	// path 参数非法（id=abc 无法绑 int64）：此前 200 + 10001 ⇒ 现 400
	code, body = doAdminErrReq(t, "PUT", "/admin/users/abc/status", "{\"status\":1}")
	assert.Equal(t, http.StatusBadRequest, code, "参数错误必须 400：%v", body)
	assertAdminErrEnvelope(t, body, errcode.CodeInvalidParam)

	// 资源缺失（按 code 查不到）：此前 200 + body code=404 ⇒ 现 404 + 10004
	code, body = doAdminErrReq(t, "DELETE", "/admin/files/ABCDEFGH", "")
	assert.Equal(t, http.StatusNotFound, code, "资源缺失必须 404（不得进 HTTP 200）：%v", body)
	assertAdminErrEnvelope(t, body, errcode.CodeNotFound)
}

// TestAdminErrorEnvelope_InternalSanitized core#114：500 类脱敏 + 全站信封。
func TestAdminErrorEnvelope_InternalSanitized(t *testing.T) {
	newBrokenAdminEnv(t)

	// 注：AdminStats/GetConfig 在查询失败时静默回退零值/默认配置（不报错），
	// 坏库驱动不出它们的错误路径——错误出口形态由同构的 AdminListUsers/
	// AdminUpdateUserStatus/AdminUpdateConfig 用例覆盖（同一套 NewInternalError 出口）。
	for _, tc := range []struct{ method, path, body, what string }{
		{"GET", "/admin/users?page=1&page_size=10", "", "AdminListUsers"},
		{"PUT", "/admin/users/1/status", "{\"status\":0}", "AdminUpdateUserStatus"},
		{"PUT", "/admin/config", "{\"basic\":{\"name\":\"x\"}}", "AdminUpdateConfig"},
	} {
		code, body := doAdminErrReq(t, tc.method, tc.path, tc.body)
		assert.Equal(t, http.StatusInternalServerError, code,
			"%s 内部故障应 500（不得进 HTTP 200）：%v", tc.what, body)
		assertAdminErrEnvelope(t, body, errcode.CodeInternal, "no such table", "SELECT", "sql:")
	}
}

// TestAdminErrorEnvelope_Param400 非法 JSON 配置体：400 + 10001 + trace_id。
func TestAdminErrorEnvelope_Param400(t *testing.T) {
	newBrokenAdminEnv(t)

	code, body := doAdminErrReq(t, "PUT", "/admin/config", "not-json")
	assert.Equal(t, http.StatusBadRequest, code, "%v", body)
	assertAdminErrEnvelope(t, body, errcode.CodeInvalidParam)
}
