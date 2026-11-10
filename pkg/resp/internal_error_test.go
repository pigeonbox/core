package resp

import (
	stderrors "errors"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/stretchr/testify/assert"

	"github.com/pigeonbox/contracts/errcode"
)

// TestNewInternalError_Sanitizes 内部错误统一出口（core#114）：
//   - err 原文（SQL/驱动文本）**不得**出网（只进日志）；
//   - 对外是脱敏文案 + 业务码 + trace_id，HTTP 状态走业务码映射（500 类 ⇒ 500）。
//
// 负对照（已实跑）：把 NewInternalError 改回 NewErrorWithMessage(c, code, err.Error())
// ⇒ 本用例的 NotContains / message 断言转红。
func TestNewInternalError_Sanitizes(t *testing.T) {
	raw := stderrors.New("no such column: secret_col (SELECT * FROM `users`)")
	c := app.NewContext(1)
	NewInternalError(c, errcode.CodeDBError, "查询公告列表", raw)

	assert.Equal(t, consts.StatusInternalServerError, c.Response.StatusCode(),
		"500 类业务码必须映射 HTTP 500")
	body := string(c.Response.Body())
	assert.Contains(t, body, `"code":50001`)
	assert.NotContains(t, body, "no such column", "内部错误细节不得出网")
	assert.NotContains(t, body, "SELECT", "SQL 文本不得出网")
	assert.Contains(t, body, `"trace_id"`, "错误响应必须带 trace_id 可对账")
	assert.Contains(t, body, InternalFailureMsg("查询公告列表"),
		"对外文案是脱敏文案（op 语义保留、细节丢弃）")
}

// TestInternalFailureMsg 文案契约：op 保留操作语义；空 op 回通用文案。
func TestInternalFailureMsg(t *testing.T) {
	assert.Equal(t, "操作失败，请稍后重试", InternalFailureMsg(""))
	assert.Equal(t, "删除失败，请稍后重试", InternalFailureMsg("删除"))
}

// TestHTTPStatusForCode_InternalIs500 10008「内部错误」必须 500（core#114）。
//
// 缺陷史：CodeInternal=10008 落在 10001–10999 段 ⇒ IsClientError 命中 ⇒
// 映射成 **HTTP 400**（「服务端坏了」报成「参数传错」）。补显式 case 后回 500。
// 负对照（已实跑）：去掉 response.go 里 CodeInternal 的显式 case ⇒ 本用例红（400）。
func TestHTTPStatusForCode_InternalIs500(t *testing.T) {
	assert.Equal(t, consts.StatusInternalServerError, httpStatusForCode(errcode.CodeInternal))
	assert.Equal(t, consts.StatusInternalServerError, httpStatusForCode(errcode.CodeDBError))
}
