package audit

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/pkg/auth"
	"github.com/pigeonbox/core/pkg/middleware"
)

// captureSink 测试桩：捕获 Record 落盘的 Entry 与 ctx（生产由 bootstrap 以
// dao 桥注入，见 bootstrap/adapters.go daoAuditSink）。
type captureSink struct {
	entries []Entry
	ctxs    []context.Context
	err     error
}

func (s *captureSink) Create(ctx context.Context, e Entry) error {
	s.ctxs = append(s.ctxs, ctx)
	s.entries = append(s.entries, e)
	return s.err
}

// TestRecord_SinkNotWiredDropsWithoutPanic sink 未装配（装配错误形态）：记录
// 丢弃但绝不 panic——审计降级不阻断业务（与收口前两处实现的降级口径一致）。
func TestRecord_SinkNotWiredDropsWithoutPanic(t *testing.T) {
	SetSink(nil)
	t.Cleanup(func() { SetSink(nil) })

	require.NotPanics(t, func() {
		Record(context.Background(), "user.delete", "user 1", true)
	})
}

// TestRecord_AnonymousCtxActorSystem 匿名/非请求 ctx：操作者记 system（定时
// 任务等无身份场景），Action/Target/Success 逐字透传，ctx 原样交给 sink。
func TestRecord_AnonymousCtxActorSystem(t *testing.T) {
	s := &captureSink{}
	SetSink(s)
	t.Cleanup(func() { SetSink(nil) })

	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "v")
	Record(ctx, "maintenance.clean_expired_files", "expired=12", false)

	require.Len(t, s.entries, 1)
	e := s.entries[0]
	assert.Equal(t, "maintenance.clean_expired_files", e.Action)
	assert.Equal(t, "expired=12", e.Target)
	assert.False(t, e.Success)
	assert.Nil(t, e.ActorID, "匿名操作者 ActorID 必须为 nil（不伪造 0 号用户）")
	assert.Equal(t, "system", e.ActorName)
	assert.Equal(t, "", e.IP)
	require.Len(t, s.ctxs, 1)
	assert.Equal(t, "v", s.ctxs[0].Value(ctxKey{}), "sink 必须收到调用方的 ctx（trace_id 等链路值不丢）")
}

// TestRecord_AuthenticatedRequestActor 真实认证请求链：操作者经
// middleware.AuthMiddleware 注入的身份提取（ID/用户名/IP 三件套）。
func TestRecord_AuthenticatedRequestActor(t *testing.T) {
	auth.SetJWTSecret("test-secret-for-audit-record-32chars")
	t.Cleanup(func() { auth.SetJWTSecret("") })

	s := &captureSink{}
	SetSink(s)
	t.Cleanup(func() { SetSink(nil) })

	token, err := auth.GenerateToken(7, "bob", "user")
	require.NoError(t, err)

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.POST("/audit-probe", middleware.AuthMiddleware(),
		func(ctx context.Context, c *app.RequestContext) {
			Record(ctx, "user.update", "user 7 updated", true)
			c.JSON(http.StatusOK, map[string]string{})
		})

	w := ut.PerformRequest(h.Engine, http.MethodPost, "/audit-probe", nil,
		ut.Header{Key: "Authorization", Value: "Bearer " + token})
	require.Equal(t, http.StatusOK, w.Result().StatusCode())

	require.Len(t, s.entries, 1)
	e := s.entries[0]
	assert.Equal(t, "user.update", e.Action)
	require.NotNil(t, e.ActorID, "登录操作必须落 ActorID 归因")
	assert.Equal(t, uint(7), *e.ActorID)
	assert.Equal(t, "bob", e.ActorName)
	assert.NotEmpty(t, e.IP, "请求场景必须落客户端 IP")
}

// TestRecord_SinkErrorSwallowed 落盘失败：Record 不向调用方冒泡错误/不 panic
// （审计失败只记运行日志，绝不阻断业务——收口前两处实现同款口径）。
func TestRecord_SinkErrorSwallowed(t *testing.T) {
	s := &captureSink{err: errors.New("table missing")}
	SetSink(s)
	t.Cleanup(func() { SetSink(nil) })

	require.NotPanics(t, func() {
		Record(context.Background(), "config.update", "system config persisted to database", true)
	})
	assert.Len(t, s.entries, 1, "失败也必须尝试落盘一次（告警在执行点内）")
}
