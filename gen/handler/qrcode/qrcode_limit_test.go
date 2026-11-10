package qrcode

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"

	qrcodeservice "github.com/pigeonbox/core/app/qrcode"
)

// core#109 handler 层判据：/qrcode/generate 对超界 size/data 回 **400 + 业务码
// 10001 + 消息点名参数**（全站信封），而不是放行进生成器（size 无上限 ⇒
// 匿名单请求 ~1.6GB RGBA 分配 = 单请求内存 DoS）。
//
// 负对照（当场可复验）：把 ValidateGenerateParams 调用从 handler 摘掉 ⇒
// 下列 400 断言全部转红（20000 的请求会被真送去生成器）。

func newQRContext(t *testing.T, body string) *app.RequestContext {
	t.Helper()
	c := app.NewContext(0)
	c.Request.Header.SetMethod("POST")
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.SetBody([]byte(body))
	// hz binder 的 preBindBody 按 Content-Length>0 判定是否有 body——手工 SetBody
	// 不会补这个头，不设则 JSON 静默绑不上（实测踩过）。
	c.Request.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return c
}

type qrErrBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	TraceID string `json:"trace_id"`
}

// TestGenerateQRCodeHandler_OversizeRejected 超界取值 ⇒ 400 + 10001 + 点名参数。
func TestGenerateQRCodeHandler_OversizeRejected(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		errPart string
	}{
		{
			"size=20000 攻击形态",
			`{"data":"https://example.com","size":20000,"return_base64":false}`,
			"size",
		},
		{
			"size 负数（修前静默归 256）",
			`{"data":"https://example.com","size":-5,"return_base64":false}`,
			"size",
		},
		{
			"data 超 Medium 容量（修前塌成 500）",
			`{"data":"` + strings.Repeat("a", qrcodeservice.MaxDataBytes+1) + `","size":256,"return_base64":false}`,
			"data",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newQRContext(t, tc.body)
			GenerateQRCode(context.Background(), c)
			if got := c.Response.StatusCode(); got != 400 {
				t.Fatalf("HTTP 状态应 400，实得 %d（body %s）", got, c.Response.Body())
			}
			var body qrErrBody
			if err := json.Unmarshal(c.Response.Body(), &body); err != nil {
				t.Fatalf("错误响应应是 JSON 信封: %v", err)
			}
			if body.Code != 10001 {
				t.Fatalf("业务码应 10001（参数错误），实得 %d", body.Code)
			}
			if !strings.Contains(body.Message, tc.errPart) {
				t.Fatalf("消息应点名参数 %q，实得 %q", tc.errPart, body.Message)
			}
			if body.TraceID == "" {
				t.Fatal("全站信封应带 trace_id")
			}
		})
	}
}

// TestGenerateQRCodeHandler_ValidOK 合法请求照常 200 出图（成功 wire 形状不动）。
func TestGenerateQRCodeHandler_ValidOK(t *testing.T) {
	c := newQRContext(t, `{"data":"https://example.com","size":256,"return_base64":false}`)
	GenerateQRCode(context.Background(), c)
	if got := c.Response.StatusCode(); got != 200 {
		t.Fatalf("合法请求应 200，实得 %d（body %s）", got, c.Response.Body())
	}
	var out map[string]any
	if err := json.Unmarshal(c.Response.Body(), &out); err != nil {
		t.Fatalf("响应应是 JSON: %v", err)
	}
	if out["code"] != float64(0) {
		t.Fatalf("成功业务码应 0，实得 %v", out["code"])
	}
	data, _ := out["data"].(map[string]any)
	if data == nil || !strings.HasPrefix(data["id"].(string), "qr_") {
		t.Fatalf("data.id 应为 qr_ 前缀，实得 %v", out["data"])
	}
}
