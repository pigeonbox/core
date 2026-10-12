package bootstrap

import (
	"context"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/pigeonbox/core/pkg/baseurl"
	"github.com/stretchr/testify/assert"
)

// runPublicBase 跑 publicBaseMiddleware + 单个下游 handler，返回下游收到的 go ctx。
// 守卫点：推断值必须经 go ctx 到达下游（service 层读的是 handler 首参 go ctx；
// 2026-10-12 前中间件用 c.Set 只落 RequestContext.Keys，推断为死代码）。
func runPublicBase(t *testing.T, host, proto string) context.Context {
	t.Helper()
	c := app.NewContext(0)
	if host != "" {
		c.Request.SetHost(host)
	}
	if proto != "" {
		c.Request.Header.Set("X-Forwarded-Proto", proto)
	}
	var got context.Context
	c.SetHandlers(app.HandlersChain{
		publicBaseMiddleware(),
		func(ctx context.Context, rc *app.RequestContext) { got = ctx },
	})
	c.Next(context.Background())
	assert.NotNil(t, got, "中间件不得中断链（下游 handler 必须被执行）")
	return got
}

func TestPublicBaseMiddleware_FollowsRequestHost(t *testing.T) {
	got := runPublicBase(t, "home.enjoye.top:56789", "")
	assert.Equal(t, "http://home.enjoye.top:56789", baseurl.Resolve(got, ""))
}

func TestPublicBaseMiddleware_HonorsForwardedProto(t *testing.T) {
	got := runPublicBase(t, "s.example.com", "https")
	assert.Equal(t, "https://s.example.com", baseurl.Resolve(got, ""))
}

func TestPublicBaseMiddleware_EmptyHostFallsBackRelative(t *testing.T) {
	got := runPublicBase(t, "", "")
	// 双缺省退相对路径：浏览器侧由 location.origin 补全（baseurl 包契约）
	assert.Equal(t, "", baseurl.Resolve(got, ""))
}

func TestPublicBaseMiddleware_ConfiguredWins(t *testing.T) {
	got := runPublicBase(t, "lan.local:8080", "")
	// 显式配置优先级高于请求来源（baseurl 包契约）
	assert.Equal(t, "https://pub.example.com", baseurl.Resolve(got, "https://pub.example.com"))
}
