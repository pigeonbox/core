package handler

// POST /admin/notify/channels/test 的 handler 层判据（core#122）：
//
//   - 请求非法（非法 JSON / 缺 channel / 未知渠道）⇒ 400 + 业务码 10001
//     全站信封（{code,message,trace_id}，core#102/#104 口径）；
//   - 发送成功/失败 ⇒ HTTP 200 + 信封，data 恰为 {ok, latency_ms,
//     error_summary} 三键（D2 按此消费）；
//   - 渠道失败（含未配置）不是 API 错误 ⇒ 仍 200，ok=false。
//
// 断言结构事实（状态/业务码/键集合），不依赖文案。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	notifyapp "github.com/pigeonbox/core/app/notify"
	"github.com/pigeonbox/core/conf"
)

func newChannelTestEngine(t *testing.T) *server.Hertz {
	t.Helper()
	SetNotifyService(notifyapp.NewService(nil))
	t.Cleanup(func() { SetNotifyService(nil) })
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.POST("/admin/notify/channels/test", AdminTestNotifyChannel)
	return h
}

func decodeEnvelope(t *testing.T, body []byte) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(body, &m))
	return m
}

// TestAdminTestChannel_InvalidRequests 三类请求非法 ⇒ 400 + 10001 + 信封。
func TestAdminTestChannel_InvalidRequests(t *testing.T) {
	h := newChannelTestEngine(t)
	cases := []struct {
		name string
		body string
	}{
		{"非法JSON", "{"},
		{"缺channel", "{}"},
		{"未知渠道", "{\"channel\":\"sms\"}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := ut.PerformRequest(h.Engine, "POST", "/admin/notify/channels/test",
				&ut.Body{Body: strings.NewReader(tc.body), Len: len(tc.body)},
				ut.Header{Key: "Host", Value: "test.local"},
				ut.Header{Key: "Content-Type", Value: "application/json"})
			assert.Equal(t, http.StatusBadRequest, w.Code)
			env := decodeEnvelope(t, w.Body.Bytes())
			assert.Equal(t, float64(10001), env["code"], "参数错误业务码 10001")
			assert.NotEmpty(t, env["trace_id"], "错误信封带 trace_id")
			assert.Nil(t, env["data"], "参数错误不带 data")
		})
	}
}

// TestAdminTestChannel_NotConfigured 渠道未配置 ⇒ 200 + ok=false（测试结论非 API 错误）。
func TestAdminTestChannel_NotConfigured(t *testing.T) {
	h := newChannelTestEngine(t)
	body := "{\"channel\":\"webhook\"}"
	w := ut.PerformRequest(h.Engine, "POST", "/admin/notify/channels/test",
		&ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Host", Value: "test.local"},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	require.Equal(t, http.StatusOK, w.Code)

	env := decodeEnvelope(t, w.Body.Bytes())
	assert.Equal(t, float64(0), env["code"])
	data, ok := env["data"].(map[string]interface{})
	require.True(t, ok, "data 必须是对象")
	assert.Len(t, data, 3, "data 恰为 {ok, latency_ms, error_summary} 三键")
	assert.Contains(t, data, "ok")
	assert.Contains(t, data, "latency_ms")
	assert.Contains(t, data, "error_summary")
	assert.Equal(t, false, data["ok"])
	assert.NotEmpty(t, data["error_summary"], "失败摘要非空")
}

// TestAdminTestChannel_Success 真实发送成功 ⇒ 200 + ok=true + latency_ms。
// （webhook 渠道 + httptest 靶子；telegram/bark 的真实发送判据在 app/notify 层。）
func TestAdminTestChannel_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	// httptest 是 127.0.0.1 ⇒ 全局开 allow_private（测试专用，用完恢复）
	prev := conf.GetGlobalConfig()
	conf.SetGlobalConfig(&conf.AppConfiguration{
		Security: conf.SecurityConfig{SSRF: conf.SSRFConfig{AllowPrivateNetworks: true}},
	})
	t.Cleanup(func() { conf.SetGlobalConfig(prev) })

	h := newChannelTestEngine(t)
	getNotifyService().SetWebhookURL(srv.URL)

	body := "{\"channel\":\"webhook\"}"
	w := ut.PerformRequest(h.Engine, "POST", "/admin/notify/channels/test",
		&ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Host", Value: "test.local"},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	require.Equal(t, http.StatusOK, w.Code)

	env := decodeEnvelope(t, w.Body.Bytes())
	assert.Equal(t, float64(0), env["code"])
	data := env["data"].(map[string]interface{})
	assert.Equal(t, true, data["ok"])
	assert.Len(t, data, 3)
	_, hasLatency := data["latency_ms"].(float64)
	assert.True(t, hasLatency, "latency_ms 必须是数值")
}
