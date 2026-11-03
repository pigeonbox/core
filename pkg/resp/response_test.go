package resp

import (
	stderrors "errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	pkgerrors "github.com/pigeonbox/core/pkg/errors"
	"github.com/stretchr/testify/assert"
)

// TestValidTraceID trace_id 白名单（与 pkg/middleware.RequestID 共用）
func TestValidTraceID(t *testing.T) {
	assert.True(t, ValidTraceID("01HV2P3Q-finest_workstation.42"))
	assert.True(t, ValidTraceID(strings.Repeat("a", 64)), "恰好 64 字符合法")
	assert.False(t, ValidTraceID(""))
	assert.False(t, ValidTraceID(strings.Repeat("a", 65)), "超 64 字符拒绝")
	assert.False(t, ValidTraceID("has space"))
	assert.False(t, ValidTraceID("evil\nline"))
	assert.False(t, ValidTraceID("semi;colon"))
	assert.False(t, ValidTraceID("中文"))
}

// TestGetOrGenTraceIDSanitizes envelope 里的 trace_id 同样必须过白名单
// （generated handler 大量直接 c.JSON，不经 resp helper，但 X-Trace-Id
// 响应头由 RequestID 中间件消毒；此处守住 resp 路径自己的回显）
func TestGetOrGenTraceIDSanitizes(t *testing.T) {
	c := app.NewContext(1)
	c.Request.Header.Set(traceIDHeader, strings.Repeat("A", 800))
	tid := getOrGenTraceID(c)
	assert.Len(t, tid, 36, "不合规 trace_id 必须重新生成")
	assert.Equal(t, tid, string(c.Response.Header.Get(traceIDHeader)))
}

// ── NewTypedError 的业务码透传（2026-11-02 core#30 S27 冒烟实测暴露）────────
// pkgerrors.New 构造的 AppError（= oekit BizError）没有 ErrCode() 方法，
// 此前经 NewTypedError 一律塌成 10008「内部错误」——wrapped_key 形状非法的
// 10001 到 wire 上成了 10008，调用方无法区分传参错与服务端故障。

type fakeTypedError struct{ code int }

func (e *fakeTypedError) Error() string { return "typed" }

func (e *fakeTypedError) ErrCode() int { return e.code }

func TestNewTypedError(t *testing.T) {
	// ① 自定义 typed error（ErrCode()）按其码透传
	c := app.NewContext(1)
	NewTypedError(c, &fakeTypedError{code: 30013})
	assert.Contains(t, string(c.Response.Body()), `"code":30013`)

	// ② 业务错误（AppError/BizError）按 BizCode 透传，message 不带 biz_code 前缀
	c = app.NewContext(1)
	NewTypedError(c, pkgerrors.New(pkgerrors.CodeBadRequest, "wrapped_key 格式非法"))
	body := string(c.Response.Body())
	assert.Contains(t, body, `"code":10001`, "BizError 的业务码必须透传（修前塌成 10008）")
	assert.Contains(t, body, "wrapped_key 格式非法")
	assert.NotContains(t, body, "biz_code:", "message 应取 Message 字段，不含 Error() 的日志前缀")

	// ②b 被 fmt.Errorf 包过一层也要能提出来（errors.As 走链）
	c = app.NewContext(1)
	NewTypedError(c, fmt.Errorf("create failed: %w", pkgerrors.New(pkgerrors.CodeBadRequest, "bad wk")))
	assert.Contains(t, string(c.Response.Body()), `"code":10001`)

	// ③ 普通 error 仍按内部错误
	c = app.NewContext(1)
	NewTypedError(c, stderrors.New("boom"))
	assert.Contains(t, string(c.Response.Body()), `"code":10008`)
}
