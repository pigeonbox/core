// core#110 wire 判据：/share/multi-direct 表单通道的 expire_value 非数字/≤0
// 不再被 convert.Str2Int(.., 0) 静默当 0（day ⇒ 静默改义 1 天，用户以为 30 天；
// count ⇒ 0 = 建完即死且服务端回 success），显式 400 + 10001 + 点名
// expire_value（paging.ParamError 形态）。合法值与「未传」口径不动。
//
// 负对照（已实跑）：把 parseMultiCommonFromForm 的 ParseExpireParams 换回
// convert.Str2Int(c.DefaultPostForm("expire_value", "1"), 0) ⇒ 「应拒绝」
// 用例全部转红（回到静默改义）。
package handler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
)

func newMultiFormContext(form string) *app.RequestContext {
	c := app.NewContext(0)
	c.Request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	c.Request.SetBody([]byte(form))
	return c
}

func TestParseMultiCommonFromForm_ExpireStrict(t *testing.T) {
	t.Run("应拒绝：非数字/≤0", func(t *testing.T) {
		for _, form := range []string{
			"expire_value=abc&expire_style=day",   // issue 点名形态：修前静默 1 天
			"expire_value=abc&expire_style=count", // issue 点名形态：修前 0=建完即死
			"expire_value=&expire_style=day",      // 显式空值 = 解析失败
			"expire_value=2.5&expire_style=day",
			"expire_value=-3&expire_style=day",
			"expire_value=0&expire_style=count", // 建完即死，必须拒
		} {
			c := newMultiFormContext(form)
			if p := parseMultiCommonFromForm(context.Background(), c); p != nil {
				t.Fatalf("%q 应拒绝但放行（修前静默改义/建完即死）", form)
			}
			assertParam400(t, c, "expire_value")
		}
	})

	t.Run("放行：合法值与未传口径不动", func(t *testing.T) {
		cases := []struct {
			form      string
			wantValue int
			wantStyle string
		}{
			{"", 1, "day"}, // 完全未传 ⇒ 表单默认 1/天（历史口径不动）
			{"expire_value=3&expire_style=day", 3, "day"},
			{"expire_value=30&expire_style=day", 30, "day"},
			{"expire_value=0&expire_style=forever", 0, "forever"}, // forever 免 ≤0 检查
		}
		for _, tc := range cases {
			c := newMultiFormContext(tc.form)
			p := parseMultiCommonFromForm(context.Background(), c)
			if p == nil {
				t.Fatalf("%q 应放行但被拒（body %s）", tc.form, c.Response.Body())
			}
			if p.ExpireValue != tc.wantValue || p.ExpireStyle != tc.wantStyle {
				t.Fatalf("%q ⇒ (%d,%q)，want (%d,%q)", tc.form, p.ExpireValue, p.ExpireStyle, tc.wantValue, tc.wantStyle)
			}
		}
	})
}

// TestParseMultiCommonFromForm_ErrorMessageNamesParam 错误消息点名参数+期望
// 格式（与 core#102/#104/#123 家族同形态）。
func TestParseMultiCommonFromForm_ErrorMessageNamesParam(t *testing.T) {
	c := newMultiFormContext("expire_value=abc&expire_style=day")
	parseMultiCommonFromForm(context.Background(), c)
	var body struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(c.Response.Body(), &body); err != nil {
		t.Fatalf("响应应是 JSON: %v", err)
	}
	if body.Code != 10001 {
		t.Fatalf("业务码应 10001，实得 %d", body.Code)
	}
	if !strings.Contains(body.Message, "expire_value") {
		t.Fatalf("消息应点名 expire_value，实得 %q", body.Message)
	}
}
