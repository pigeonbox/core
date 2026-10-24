// 匿名下载直链面（GET /anonymous/download/:code）的「次数耗尽」闸门（2026-10-24）
//
// 该面是匿名取件通道的**最后一公里**：校验后 302 到 /share/download 直链。
// 额度用完后必须拒绝，且给**独立错误码** 20007「取件码已用完」——
// 不能塌成 20005「取件码不存在」（持码者会以为码写错了，真相是「来晚了」）。
//
// 缺口来源同 v0.32.4：`IsExpired()` 拆分前含次数语义 ⇒ 本面天然也拒「次数
// 耗尽」；拆分后本面**漏了这道闸**（此前本域对该面**没有耗尽态测试**）。
//
// 断言必须能变红：删掉 share_anonymous_service.go 里 Download 的
// IsCountExhausted 闸 ⇒ 本用例红（会退回 20005/404）。
package share_anonymous

import (
	"net/url"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// performAnonDownload 调 GET /anonymous/download/:code，返回 HTTP 码与响应体。
func performAnonDownload(t *testing.T, code string) (int, string) {
	t.Helper()
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.GET("/anonymous/download/:code", Download)
	w := ut.PerformRequest(h.Engine, "GET", "/anonymous/download/"+url.PathEscape(code), nil,
		ut.Header{Key: "Host", Value: "test.local"})
	return w.Code, w.Body.String()
}

// TestAnonymousDownload_CountExhausted_ReturnsExhausted
//
// count=1 的分享：走一遍取件（retrieve → download，扣掉唯一一次额度）⇒
// 再取直链 ⇒ 20007「取件码已用完」。
func TestAnonymousDownload_CountExhausted_ReturnsExhausted(t *testing.T) {
	_, code := newAnonFlowEnv(t, 1)

	// 正向对照：额度未用完 ⇒ 直链可用（302）
	status, body := performAnonDownload(t, code)
	require.Equal(t, 302, status, "额度未用完时应下发直链（302）：%s", body)

	// 走一遍取件 ⇒ 交付时扣掉唯一一次额度
	_, _, dlCode, dlBody := performAnonFlow(t, code)
	require.Equal(t, 200, dlCode, "末次取件应交付成功：%s", dlBody)

	// 额度用完 ⇒ 20007（不是 20005「不存在」，也不是 404）
	status, body = performAnonDownload(t, code)
	assert.NotEqual(t, 302, status, "额度用完后不得再下发直链：%s", body)
	assert.Contains(t, body, "20007", "应是 20007「取件码已用完」：%s", body)
	assert.NotContains(t, body, "20005", "不得伪装成「取件码不存在」：%s", body)
}
