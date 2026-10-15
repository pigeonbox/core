// ctx.go — Ctx 系日志门面：业务排障日志带 trace_id 的一等入口。
//
// 痛点（可观测评估 2026-10-16）：app 层业务日志 48 处 0 trace_id，排障只能靠
// accesslog 兜。trace_id 写入点在 pkg/middleware.RequestID（ctx key
// CtxKeyTraceID），但 middleware 已依赖 logger（accesslog/ratelimit/recovery），
// logger 不能反向 import——故 ctx key 真相源下沉本包，middleware 以类型别名
// 复用（Go 类型同一性保证 WithValue/Value 配对，既有写入零迁移）。
package logger

import (
	"context"
	"fmt"

	"go.uber.org/zap"
)

// CtxKeyTraceID context.Context 中 trace_id 的键（全仓唯一真相源；
// pkg/middleware 经类型别名 `CtxKeyTraceID = logger.CtxKeyTraceID` 复用）。
type CtxKeyTraceID struct{}

// TraceIDFromContext 从 context.Context 提取 trace_id，无则空串。
func TraceIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(CtxKeyTraceID{}).(string); ok {
		return v
	}
	return ""
}

// withTraceID ctx 里带 trace_id 则附 trace_id 字段，无则退化普通 logger
// （不造空字段噪音；后台定时任务无请求链路时即此路径）。
func withTraceID(ctx context.Context) *zap.Logger {
	l := get()
	if tid := TraceIDFromContext(ctx); tid != "" {
		return l.With(zap.String("trace_id", tid))
	}
	return l
}

// kvFields 把 slog 风格交替 kv 转成 zap 字段；奇数尾巴按 zap 惯例记 "odd"。
func kvFields(kv []any) []zap.Field {
	fields := make([]zap.Field, 0, len(kv)/2+1)
	for i := 0; i+1 < len(kv); i += 2 {
		key, ok := kv[i].(string)
		if !ok {
			key = fmt.Sprint(kv[i])
		}
		fields = append(fields, zap.Any(key, kv[i+1]))
	}
	if len(kv)%2 == 1 {
		fields = append(fields, zap.Any("odd", kv[len(kv)-1]))
	}
	return fields
}

// CtxInfo / CtxWarn / CtxError：排障日志入口——ctx 里带 trace_id（请求链路）
// 时自动附 trace_id 字段，与访问日志 / 响应包络同一真相源；无链路 ctx 的
// 调用点行为退化为普通 Info/Warn/Error。caller 归因与 Debug/Info 等门面
// 同深（facade 体直呼 zap，Init 的 AddCallerSkip(1) 同样生效）。
func CtxInfo(ctx context.Context, msg string, kv ...any) {
	withTraceID(ctx).Info(msg, kvFields(kv)...)
}

func CtxWarn(ctx context.Context, msg string, kv ...any) {
	withTraceID(ctx).Warn(msg, kvFields(kv)...)
}

func CtxError(ctx context.Context, msg string, kv ...any) {
	withTraceID(ctx).Error(msg, kvFields(kv)...)
}
