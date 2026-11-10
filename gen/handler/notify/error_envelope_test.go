// 错误信封统一（core#114）：gen/handler/notify 的 500 类错误此前把 err.Error()
// 原文出网（CodeDBError 泄漏 + legacy map 无 trace_id 两形态并存）。
// 判据同 request 包：500 类脱敏 + 全站信封；业务/参数类保留文案与状态。
//
// 负对照（已实跑）：把 Mine 的内部错误响应改回
// c.JSON(500, map{..., "message": err.Error()}) ⇒ InternalSanitized 用例转红。
package notify

import (
	"net/http"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/contracts/errcode"
	notifyApp "github.com/pigeonbox/core/app/notify"
)

// newBrokenNotifyEnv service 绑到**未建表**的内存库 ⇒ 查询稳定失败（内部错误路径）。
func newBrokenNotifyEnv(t *testing.T) {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	SetNotifyService(notifyApp.NewService(g))
	t.Cleanup(func() { SetNotifyService(nil) })
}

func notifyTopKeys(body map[string]any) []string {
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	return keys
}

// assertNotifyErrEnvelope 全站信封判据（结构性：键集合/trace_id/无内部串）。
func assertNotifyErrEnvelope(t *testing.T, body map[string]any, wantCode int, internalMarkers ...string) {
	t.Helper()
	assert.Equal(t, float64(wantCode), body["code"], "业务码：%v", body)
	assert.NotEmpty(t, body["trace_id"], "错误响应必须带 trace_id 可对账：%v", body)
	assert.NotEmpty(t, body["message"], "%v", body)
	msg, _ := body["message"].(string)
	for _, m := range internalMarkers {
		assert.NotContains(t, msg, m, "内部错误细节不得出网：%v", body)
	}
	assert.ElementsMatch(t, []string{"code", "message", "trace_id"}, notifyTopKeys(body),
		"错误响应应是全站信封：%v", body)
}

// TestNotifyErrorEnvelope_InternalSanitized 500 类：不进 HTTP 200、细节不出网。
func TestNotifyErrorEnvelope_InternalSanitized(t *testing.T) {
	newBrokenNotifyEnv(t)

	for _, tc := range []struct{ method, path, body, what string }{
		{"GET", "/api/v1/notifies/mine", "", "Mine"},
		{"GET", "/api/v1/notifies/unread-count", "", "UnreadCount"},
		{"POST", "/api/v1/notifies/mark-read", "{\"all\":true}", "MarkRead"},
	} {
		code, body := doNotifyReq(t, tc.method, tc.path, tc.body, 7)
		assert.Equal(t, http.StatusInternalServerError, code,
			"%s 内部故障应 500（不得进 HTTP 200）：%v", tc.what, body)
		assertNotifyErrEnvelope(t, body, errcode.CodeInternal, "no such table", "SELECT", "sql:")
	}
}

// TestNotifyErrorEnvelope_Param400 参数类：400 + 10001 + trace_id。
// （MarkRead 的「仅支持 all=true」此前是 legacy map 无 trace_id——方言回潮判据。）
func TestNotifyErrorEnvelope_Param400(t *testing.T) {
	newBrokenNotifyEnv(t)

	code, body := doNotifyReq(t, "POST", "/api/v1/notifies/mark-read", "{}", 7)
	assert.Equal(t, http.StatusBadRequest, code, "%v", body)
	assertNotifyErrEnvelope(t, body, errcode.CodeInvalidParam)
}
