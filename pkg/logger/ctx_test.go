package logger

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// withObservedLogger 以 observer core 替换全局 Logger（用例结束还原），
// 返回捕获器。不动 once：Logger 非 nil 时 get() 直接沿用。
func withObservedLogger(t *testing.T) *observer.ObservedLogs {
	t.Helper()
	old := Logger
	core, logs := observer.New(zapcore.InfoLevel)
	Logger = zap.New(core)
	t.Cleanup(func() { Logger = old })
	return logs
}

func TestCtxWarn_AttachesTraceIDWhenPresent(t *testing.T) {
	logs := withObservedLogger(t)
	ctx := context.WithValue(context.Background(), CtxKeyTraceID{}, "tid-123")

	CtxWarn(ctx, "blocked", "ip", "1.2.3.4", "scope", "login")

	entries := logs.All()
	require.Len(t, entries, 1)
	e := entries[0]
	assert.Equal(t, zapcore.WarnLevel, e.Level)
	assert.Equal(t, "blocked", e.Message)
	assert.Equal(t, "tid-123", e.ContextMap()["trace_id"], "须与访问日志同一 trace_id 真相源")
	assert.Equal(t, "1.2.3.4", e.ContextMap()["ip"])
	assert.Equal(t, "login", e.ContextMap()["scope"])
}

func TestCtxWarn_DegradesWithoutTraceID(t *testing.T) {
	logs := withObservedLogger(t)

	CtxWarn(context.Background(), "degraded", "reason", "no-chain")

	entries := logs.All()
	require.Len(t, entries, 1)
	e := entries[0]
	assert.Equal(t, "degraded", e.Message)
	_, has := e.ContextMap()["trace_id"]
	assert.False(t, has, "无链路 ctx 不得造空 trace_id 字段")
	assert.Equal(t, "no-chain", e.ContextMap()["reason"])
}

func TestCtxError_KvPairingAndOddTail(t *testing.T) {
	logs := withObservedLogger(t)
	ctx := context.WithValue(context.Background(), CtxKeyTraceID{}, "tid-err")

	CtxError(ctx, "flush failed", "attempt", 3, "err", "boom", "dangling")

	entries := logs.All()
	require.Len(t, entries, 1)
	m := entries[0].ContextMap()
	assert.Equal(t, zapcore.ErrorLevel, entries[0].Level)
	assert.Equal(t, "tid-err", m["trace_id"])
	assert.Equal(t, int64(3), m["attempt"], "zap.Any 对 int 记为 int64")
	assert.Equal(t, "boom", m["err"])
	assert.Equal(t, "dangling", m["odd"], "奇数尾巴按 zap 惯例记 odd")
}

func TestCtxInfo_NilCtxSafe(t *testing.T) {
	logs := withObservedLogger(t)

	assert.NotPanics(t, func() { CtxInfo(nil, "nil ctx ok") })
	assert.NotPanics(t, func() { assert.Equal(t, "", TraceIDFromContext(nil)) })

	entries := logs.All()
	require.Len(t, entries, 1)
	assert.Equal(t, "nil ctx ok", entries[0].Message)
}

// TestCtxKeyAliasedWithMiddleware 真相源下沉后 middleware 侧别名必须与
// logger 侧同一类型（WithContext 写入 / TraceIDFromContext 读出配对）。
func TestCtxKeyAliasedWithMiddleware(t *testing.T) {
	ctx := context.WithValue(context.Background(), CtxKeyTraceID{}, "alias-check")
	assert.Equal(t, "alias-check", TraceIDFromContext(ctx))
}
