package middleware

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/google/uuid"
	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/pkg/resp"
)

// TraceIDHeader 请求链路 ID 透传 header，与 internal/pkg/resp 对齐。
const TraceIDHeader = "X-Trace-Id"

// CtxKeyTraceID context.Context 中 trace_id 的键。
//
// 2026-10-16 起真相源下沉 pkg/logger（本包依赖 logger，反向 import 成环），
// 此处类型别名复用：类型同一性保证 WithValue/Value 配对，历史写入零迁移。
type CtxKeyTraceID = logger.CtxKeyTraceID

// RequestID 为每个请求生成 / 透传 trace_id（X-Trace-Id）。
//
// 行为：
//   - 沿用请求头 X-Trace-Id（须通过白名单校验：长度 ≤64、字符集
//     [A-Za-z0-9._-]，见 resp.ValidTraceID。2026-10-06 审计：原样回显
//     任意值会把超长串写进响应头并落入访问日志——日志刷量/注入面）
//   - 缺失或不合规时生成 UUID
//   - 写入响应头、app.RequestContext（Set）与 context.Context，
//     使访问日志 / 业务日志 / resp envelope 都能拿到同一个 trace_id。
//
// 与 resp.getOrGenTraceID 配合：resp 包在构造响应体时会读取同样的
// header（同样过白名单），保证全链路 trace_id 一致。
func RequestID() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		tid := string(c.GetHeader(TraceIDHeader))
		if !resp.ValidTraceID(tid) {
			tid = uuid.New().String()
		}
		// 写响应头，供客户端 / 代理记录
		c.Response.Header.Set(TraceIDHeader, tid)
		// 写入 RequestContext，handler 可通过 c.Get 取用
		c.Set("trace_id", tid)
		// 写入 context.Context，下游 service / logger 可取出
		c.Set("ctx", context.WithValue(ctx, CtxKeyTraceID{}, tid))

		c.Next(ctx)
	}
}

// TraceIDFromContext 从 context.Context 提取 trace_id（service / 日志用）。
// 委托 logger 同名函数（真相源），保留本包导出兼容既有调用点。
func TraceIDFromContext(ctx context.Context) string {
	return logger.TraceIDFromContext(ctx)
}
