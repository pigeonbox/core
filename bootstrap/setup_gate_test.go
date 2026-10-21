// setup_gate_test.go —— 首启门禁（可选令牌 + 按 IP 失败锁定）的行为断言。
//
// 守卫点（三个，缺一即失效）：
//  1. **不设令牌时不得拦**（保持「打开浏览器走完首启」的零摩擦体验）；
//  2. 设令牌后，**令牌不匹配必须被拒**，且失败计入按 IP 锁定；
//  3. 锁定后即使令牌正确也要拒（否则锁定形同虚设）。
package bootstrap

import (
	"context"
	"net/http"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/stretchr/testify/assert"
)

// runSetupGate 跑 setupGate + 单个下游 handler，返回「下游是否被执行」与响应码。
func runSetupGate(t *testing.T, method, path, headerToken, queryToken string) (bool, int) {
	t.Helper()
	c := app.NewContext(0)
	c.Request.SetMethod(method)
	c.Request.URI().SetPath(path)
	if headerToken != "" {
		c.Request.Header.Set(setupTokenHeader, headerToken)
	}
	if queryToken != "" {
		c.Request.URI().SetQueryString("token=" + queryToken)
	}
	reached := false
	c.SetHandlers(app.HandlersChain{
		setupGate(),
		func(ctx context.Context, rc *app.RequestContext) { reached = true },
	})
	c.Next(context.Background())
	return reached, c.Response.StatusCode()
}

func TestSetupGate_NoTokenConfigured_PassesThrough(t *testing.T) {
	t.Setenv(setupTokenEnv, "")
	reached, _ := runSetupGate(t, http.MethodPost, "/setup", "", "")
	assert.True(t, reached, "未配置令牌时不得拦首启（默认体验零摩擦）")
}

func TestSetupGate_CheckEndpointNotGated(t *testing.T) {
	t.Setenv(setupTokenEnv, "secret-token")
	// /setup/check 是无害读（前端首屏靠它决定跳不跳转向导）→ 不得拦
	reached, _ := runSetupGate(t, http.MethodGet, "/setup/check", "", "")
	assert.True(t, reached, "/setup/check 不得被令牌门拦（否则首屏直接锁死）")
}

func TestSetupGate_TokenMismatchRejected(t *testing.T) {
	t.Setenv(setupTokenEnv, "right-token")
	reached, code := runSetupGate(t, http.MethodPost, "/setup", "wrong-token", "")
	assert.False(t, reached, "令牌不匹配必须拦下")
	assert.Equal(t, http.StatusUnauthorized, code)
}

func TestSetupGate_TokenFromQueryAccepted(t *testing.T) {
	t.Setenv(setupTokenEnv, "right-token")
	// ?token= 形态：从启动日志复制链接直接打开的场景
	reached, _ := runSetupGate(t, http.MethodPost, "/setup", "", "right-token")
	assert.True(t, reached, "?token= 形态应被接受")
}

func TestSetupGate_TokenFromCookieAccepted(t *testing.T) {
	t.Setenv(setupTokenEnv, "right-token")
	// Cookie 形态：前端向导用（避免为了传一个值去改 IDL 契约的请求体）
	c := app.NewContext(0)
	c.Request.SetMethod(http.MethodPost)
	c.Request.URI().SetPath("/setup")
	c.Request.Header.SetCookie(setupTokenCookie, "right-token")
	reached := false
	c.SetHandlers(app.HandlersChain{
		setupGate(),
		func(ctx context.Context, rc *app.RequestContext) { reached = true },
	})
	c.Next(context.Background())
	assert.True(t, reached, "Cookie 形态应被接受")
}

func TestSetupGate_TokenMatchAccepted(t *testing.T) {
	t.Setenv(setupTokenEnv, "right-token")
	reached, _ := runSetupGate(t, http.MethodPost, "/setup", "right-token", "")
	assert.True(t, reached, "令牌正确必须放行")
}

func TestSetupGate_MissingTokenRejected(t *testing.T) {
	t.Setenv(setupTokenEnv, "right-token")
	reached, code := runSetupGate(t, http.MethodPost, "/setup", "", "")
	assert.False(t, reached, "缺令牌必须拦下")
	assert.Equal(t, http.StatusUnauthorized, code)
}

func TestSetupGate_FailuresLockOutByIP(t *testing.T) {
	t.Setenv(setupTokenEnv, "right-token")
	lock := middleware.GetDefaultLockout()
	key := middleware.FormatLockKey("setup", "203.0.113.9")
	lock.Reset(context.Background(), key) // 前置清理，避免受其它用例影响

	// 连续失败直到触发锁定（配置阈值 MaxAttempts 次）
	locked := false
	for i := 0; i < 20 && !locked; i++ {
		reached, code := runSetupGate(t, http.MethodPost, "/setup", "wrong-token", "")
		assert.False(t, reached, "失败尝试不得放行")
		if code == http.StatusTooManyRequests {
			locked = true
		}
	}
	assert.True(t, locked, "连续失败必须触发按 IP 锁定")

	// 锁定后即使令牌正确也要拒 —— 否则锁定形同虚设
	reached, code := runSetupGate(t, http.MethodPost, "/setup", "right-token", "")
	assert.False(t, reached, "锁定期间即使令牌正确也不得放行")
	assert.Equal(t, http.StatusTooManyRequests, code)

	lock.Reset(context.Background(), key) // 收尾清理
}
