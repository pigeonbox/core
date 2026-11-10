// 错误信封统一（core#114）：gen/handler/request 的错误响应此前是 legacy map
// （无 trace_id），且 500 类把 err.Error() 原文出网（SQL/表名可探测库结构）。
// 本组用例钉住三件事：
//
//	① 500 类：HTTP 500 + 全站信封（code/message/trace_id）+ 内部错误细节不出网；
//	② 业务类：错误码/HTTP 状态/业务文案保留（脱敏只针对内部故障）；
//	③ 参数类：400 + 10001 + trace_id。
//
// 负对照（已实跑）：把 ListMine 的内部错误响应改回
// c.JSON(500, map{..., "message": err.Error()}) ⇒ ① 的 trace_id/NotContains 转红。
package request

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
	requestApp "github.com/pigeonbox/core/app/request"
)

// newBrokenRequestEnv 把 service 绑到**未建表**的内存库 ⇒ 一切查询以
// "no such table" 失败，稳定驱动内部错误路径（不依赖注入 mock）。
func newBrokenRequestEnv(t *testing.T) {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	SetRequestService(requestApp.NewService(g, nil, nil))
	t.Cleanup(func() { SetRequestService(nil) })
}

// assertReqErrEnvelope 断言错误响应是全站信封（core#114 判据）：
// 顶层键恰为 {code,message,trace_id}、trace_id 非空、内部错误标记不出网。
func assertReqErrEnvelope(t *testing.T, body map[string]any, wantCode int, internalMarkers ...string) {
	t.Helper()
	assert.Equal(t, float64(wantCode), body["code"], "业务码：%v", body)
	assert.NotEmpty(t, body["trace_id"], "错误响应必须带 trace_id 可对账：%v", body)
	assert.NotEmpty(t, body["message"], "%v", body)
	msg, _ := body["message"].(string)
	for _, m := range internalMarkers {
		assert.NotContains(t, msg, m, "内部错误细节不得出网：%v", body)
	}
	assert.ElementsMatch(t, []string{"code", "message", "trace_id"}, reqTopKeys(body),
		"错误响应应是全站信封（多/少键都说明方言回潮）：%v", body)
}

func reqTopKeys(body map[string]any) []string {
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	return keys
}

// TestRequestErrorEnvelope_InternalSanitized 500 类：不进 HTTP 200、细节不出网。
func TestRequestErrorEnvelope_InternalSanitized(t *testing.T) {
	newBrokenRequestEnv(t)

	for _, tc := range []struct{ method, path, what string }{
		{"GET", "/api/v1/user/requests", "ListMine"},
		{"DELETE", "/api/v1/user/requests/deadbeef", "Delete"},
	} {
		code, body := doReq(t, tc.method, tc.path, "", 7)
		assert.Equal(t, http.StatusInternalServerError, code,
			"%s 内部故障应 500（不得进 HTTP 200）：%v", tc.what, body)
		assertReqErrEnvelope(t, body, errcode.CodeInternal, "no such table", "SELECT", "sql:")
	}
}

// TestRequestErrorEnvelope_BusinessKept 业务类 404：状态/业务码保留、文案可读。
func TestRequestErrorEnvelope_BusinessKept(t *testing.T) {
	newBrokenRequestEnv(t)

	code, body := doReq(t, "GET", "/request/nope", "", 0)
	assert.Equal(t, http.StatusNotFound, code, "%v", body)
	assertReqErrEnvelope(t, body, errcode.CodeNotFound)
	// 业务文案保留（不是脱敏的通用文案）：GetPublic 的既有文案以「投递链接」开头
	assert.Contains(t, body["message"], "投递链接", "业务错误应保留可操作文案：%v", body)
}

// TestRequestErrorEnvelope_Param400 参数/业务校验类：400 + 10001 + trace_id，
// 且**保留可操作文案**（脱敏只针对内部故障，不得把参数类文案也抹成通用语）。
func TestRequestErrorEnvelope_Param400(t *testing.T) {
	newBrokenRequestEnv(t)

	// Create 的 service 校验错误（投递码不支持「按取件次数」）⇒ 400 + 10001
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.POST("/api/v1/user/requests", func(ctx context.Context, c *app.RequestContext) {
		c.Set("user_id", uint(7))
		Create(ctx, c)
	})
	payload := `{"title":"t","expire_style":"count"}`
	w := ut.PerformRequest(h.Engine, "POST", "/api/v1/user/requests",
		&ut.Body{Body: strings.NewReader(payload), Len: len(payload)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)

	assert.Equal(t, http.StatusBadRequest, w.Code, "%v", body)
	assertReqErrEnvelope(t, body, errcode.CodeInvalidParam)
	msg, _ := body["message"].(string)
	assert.NotContains(t, msg, "请稍后重试", "业务/参数文案不得被脱敏成通用文案：%v", body)
}
