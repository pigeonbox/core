package admin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
)

// core#123：/admin/stats/trend 的 days 解析失败/越界**静默归 7**——
// core#102/#104「解析失败显式失败」口径的同族第 3 例。
//
// 判据：非法/越界 ⇒ 400 + 业务码 10001 + 消息点名 days；缺省仍 7。
// 负对照（当场可复验）：把 parseTrendDays 改回静默归 7 的旧实现 ⇒
// 下列「应拒绝」用例全部转红（旧行为回 200/7）。

func newTrendContext(query string) *app.RequestContext {
	c := app.NewContext(0)
	if query != "" {
		c.Request.URI().SetQueryString(query)
	}
	return c
}

func TestParseTrendDays(t *testing.T) {
	okCases := []struct {
		query string
		want  int
	}{
		{"", 7},         // 未传 ⇒ 缺省 7（历史口径不动）
		{"days=", 7},    // 空值同「没传」
		{"days=1", 1},   // 下界
		{"days=30", 30}, // 上界
		{"days=15", 15},
	}
	for _, tc := range okCases {
		t.Run("ok:"+tc.query, func(t *testing.T) {
			c := newTrendContext(tc.query)
			got, ok := parseTrendDays(c)
			if !ok {
				t.Fatalf("应放行但失败（body %s）", c.Response.Body())
			}
			if got != tc.want {
				t.Fatalf("days=%d, want %d", got, tc.want)
			}
		})
	}

	badCases := []string{
		"days=abc",   // 解析失败（修前静默 7）
		"days=7.5",   // 非整数
		"days=0",     // 越下界
		"days=-1",    // 负数
		"days=31",    // 越上界
		"days=45",    // issue 点名形态：静默少给 15 天
		"days=99999", // 越界大数
	}
	for _, q := range badCases {
		t.Run("reject:"+q, func(t *testing.T) {
			c := newTrendContext(q)
			got, ok := parseTrendDays(c)
			if ok {
				t.Fatalf("应拒绝但放行（days=%d）", got)
			}
			if code := c.Response.StatusCode(); code != 400 {
				t.Fatalf("HTTP 应 400，实得 %d（body %s）", code, c.Response.Body())
			}
			var body struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
				TraceID string `json:"trace_id"`
			}
			if err := json.Unmarshal(c.Response.Body(), &body); err != nil {
				t.Fatalf("错误响应应是 JSON 信封: %v", err)
			}
			if body.Code != 10001 {
				t.Fatalf("业务码应 10001，实得 %d", body.Code)
			}
			if !strings.Contains(body.Message, "days") {
				t.Fatalf("消息应点名参数 days，实得 %q", body.Message)
			}
			if body.TraceID == "" {
				t.Fatal("全站信封应带 trace_id")
			}
		})
	}
}

// TestAdminStatsTrend_InvalidDays400 handler 级：解析失败在触达服务前就 400 返回
// （不初始化 manageSvc 也不 panic = 未触达 getManageSvc）。
func TestAdminStatsTrend_InvalidDays400(t *testing.T) {
	c := newTrendContext("days=abc")
	AdminStatsTrend(context.Background(), c)
	if code := c.Response.StatusCode(); code != 400 {
		t.Fatalf("HTTP 应 400，实得 %d（body %s）", code, c.Response.Body())
	}
}
