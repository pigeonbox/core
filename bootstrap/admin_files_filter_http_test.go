// 管理端文件列表过滤参数的**非法值必须在 HTTP 层被拒**（core#102）。
//
// ⚠️ 这一层锁的是**对外可观察的行为**，不只是内部函数：
//
//	旧行为：非法值 ⇒ 条件被静默丢弃 ⇒ HTTP 200 + **全量**数据
//	（实测 103：created_after="+0800" ⇒ total=122 = 不带筛选时的全量）
//	⇒ 调用方以为"筛过了只是没筛中"，实际是"**根本没筛**"。
//	⚠️ 「筛选失败」比「筛选为空」危险：**筛选失败是多给数据**。
//
// 断言：HTTP 400 + 全站统一信封 {code,message,trace_id} + code=10001（参数错误）
//   - 消息点名是哪个参数。
//
// 负对照：把 handler 里的错误分支去掉（照旧"解析失败就跳过"）⇒ 本用例红。
package bootstrap

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/contracts/errcode"
	adminhandler "github.com/pigeonbox/core/gen/handler/admin"
)

// TestAdminListFilesFiltered_InvalidParamReturns400 非法过滤参数 ⇒ 400 + 10001。
func TestAdminListFilesFiltered_InvalidParamReturns400(t *testing.T) {
	cases := []struct {
		name      string
		query     string
		wantParam string
	}{
		{"非法时间", "created_after=not-a-time", "created_after"},
		{"时间偏移被空格替换", "created_after=2026-11-10T09:52:04+08:00", "created_after"},
		{"非法 user_id", "user_id=abc", "user_id"},
		{"非法 min_size", "min_size=abc", "min_size"},
		{"非法 max_size", "max_size=-1", "max_size"},
		{"非法 expired", "expired=maybe", "expired"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := server.New(server.WithHostPorts("127.0.0.1:0"))
			h.GET("/admin/files/filter", adminhandler.AdminListFilesFiltered)

			w := ut.PerformRequest(h.Engine, "GET", "/admin/files/filter?"+c.query, nil,
				ut.Header{Key: "Host", Value: "test.local"})

			require.Equal(t, 400, w.Code, "非法过滤参数必须 400（旧行为是 200 + 全量数据）")

			var body struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
				TraceID string `json:"trace_id"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			require.Equal(t, errcode.CodeInvalidParam, body.Code, "业务码必须是 10001（参数错误）")
			require.Contains(t, body.Message, c.wantParam, "消息必须点名是哪个参数")
			require.NotEmpty(t, body.TraceID, "错误响应必须带 trace_id（全站信封口径）")
			require.False(t, strings.Contains(w.Body.String(), "\"data\""),
				"错误响应不得带 data 字段（旧行为会让人以为拿到了筛选结果）")
		})
	}
}
