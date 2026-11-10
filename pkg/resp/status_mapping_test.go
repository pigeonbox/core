package resp

import (
	stderrors "errors"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/stretchr/testify/assert"

	"github.com/pigeonbox/contracts/errcode"
)

// TestHTTPStatusForCode_EnvelopeSweepMappings A6b 错误信封收编补齐的映射登记守卫。
//
// 背景：这些业务码此前落 httpStatusForCode 的 default ⇒ **HTTP 200**（业务错误
// 进 200，core#115 同族缺陷），或同一语义在不同面手写不同状态（10016 在注册面
// 409、经映射的 AdminCreateUser 却 200）。收编到统一出口后，状态只认这张表。
//
// 负对照（已实跑）：删掉 CodeConflict 的 409 case ⇒ 本用例与
// gen/handler/user/user_register_test.go 同时转红。
func TestHTTPStatusForCode_EnvelopeSweepMappings(t *testing.T) {
	cases := map[int]int{
		errcode.CodeConflict:      consts.StatusConflict,            // 10016 资源冲突 ⇒ 409
		errcode.CodeUserExists:    consts.StatusConflict,            // 40002 用户已存在 ⇒ 409（此前 default=200）
		errcode.CodeChunkInvalid:  consts.StatusUnprocessableEntity, // 20009 分片无效 ⇒ 422（前端按 422/20009 判单片可重试）
		errcode.CodeTotpRequired:  consts.StatusBadRequest,          // 40012 需要二验 ⇒ 400（不可 401：会误触发前端 refresh）
		errcode.CodeTotpInvalid:   consts.StatusBadRequest,          // 40013 二验错误 ⇒ 400
		errcode.CodeResetTokenBad: consts.StatusBadRequest,          // 40014 重置链接失效 ⇒ 400
	}
	for code, want := range cases {
		assert.Equal(t, want, httpStatusForCode(code),
			"业务码 %d 的 HTTP 状态映射不得回退（回退会让错误进 HTTP 200 或误触发会话刷新）", code)
	}
	// 同批不动的既有关键映射（回归锚点）
	assert.Equal(t, consts.StatusInternalServerError, httpStatusForCode(errcode.CodeInternal))
	assert.Equal(t, consts.StatusBadRequest, httpStatusForCode(errcode.CodeInvalidParam))
}

// TestNewTypedError_FallbackSanitizes NewTypedError 的非 typed 兜底也是内部错误 ⇒
// 同样脱敏（A6b）：此前兜底裸回 err.Error()（10008 + SQL/驱动文本出网）。
//
// 负对照（已实跑）：把 response.go 兜底改回 NewErrorWithMessage(c, CodeInternal,
// err.Error()) ⇒ 本用例 NotContains 转红。
func TestNewTypedError_FallbackSanitizes(t *testing.T) {
	raw := stderrors.New("no such table: upload_chunks (SELECT ...)")
	c := app.NewContext(1)
	NewTypedError(c, raw)

	assert.Equal(t, consts.StatusInternalServerError, c.Response.StatusCode())
	body := string(c.Response.Body())
	assert.Contains(t, body, `"code":10008`)
	assert.NotContains(t, body, "no such table", "兜底错误细节不得出网")
	assert.NotContains(t, body, "SELECT", "SQL 文本不得出网")
	assert.Contains(t, body, `"trace_id"`)
}
