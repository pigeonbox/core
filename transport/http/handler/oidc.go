// OIDC SSO HTTP 端点（P2，手写路由）。
package handler

import (
	"context"
	"fmt"
	"github.com/pigeonbox/contracts/errcode"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	oidcApp "github.com/pigeonbox/core/app/oidc"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/auth"
	"github.com/pigeonbox/core/pkg/middleware"
)

var oidcSvc *oidcApp.Service

// SetOIDCService 注入 OIDC 服务（bootstrap 调用）
func SetOIDCService(s *oidcApp.Service) { oidcSvc = s }

func getOIDCService() *oidcApp.Service { return oidcSvc }

// getOIDCBaseURL 对外基础地址（与 bootstrap 同策略：base_url 优先）
func getOIDCBaseURL() string {
	if cfg := conf.GetGlobalConfig(); cfg != nil {
		if cfg.Server.BaseURL != "" {
			return cfg.Server.BaseURL
		}
		return fmt.Sprintf("http://%s:%d", cfg.Server.Host, cfg.Server.Port)
	}
	return "http://localhost:12345"
}

// redirectOIDCError 302 回前端登录页并携带错误信息（query 传中文需转义）
func redirectOIDCError(c *app.RequestContext, msg string) {
	c.Header("Cache-Control", "no-store")
	c.Redirect(consts.StatusFound, []byte("/#/user/login?oidc_error=1"))
}

// oidcNonceCookie OIDC state 的浏览器绑定 nonce（login CSRF 防线，2026-10-08）：
// 登录起点随机下发 HttpOnly Cookie，state 的 HMAC 绑定该 nonce；回调强制回读
// 比对后立即作废——第三方拿"自己的 code+state"诱导受害者浏览器回调时，受害者
// 没有匹配的 nonce Cookie，登录 CSRF 不成立。
const oidcNonceCookie = "pb_oidc_nonce"

// setOIDCNonceCookie 下发 nonce Cookie（HttpOnly + Lax + 1h，与 state TTL 同步）
func setOIDCNonceCookie(c *app.RequestContext, nonce string) {
	secure := strings.EqualFold(string(c.Request.Header.Peek("X-Forwarded-Proto")), "https")
	c.SetCookie(oidcNonceCookie, nonce, 3600, "/", "", protocol.CookieSameSiteLaxMode, secure, true)
}

// clearOIDCNonceCookie 作废 nonce Cookie（一次性：回调无论成败都清除）
func clearOIDCNonceCookie(c *app.RequestContext) {
	c.SetCookie(oidcNonceCookie, "", -1, "/", "", protocol.CookieSameSiteLaxMode, false, true)
}

// OIDCLogin 302 跳转到 IdP 授权页（GET /api/v1/user/oidc/login）
func OIDCLogin(ctx context.Context, c *app.RequestContext) {
	svc := getOIDCService()
	if svc == nil || !svc.Enabled() {
		c.JSON(consts.StatusNotFound, map[string]interface{}{"code": errcode.CodeNotFound, "message": "OIDC 登录未启用"})
		return
	}
	nonce := oidcApp.GenerateNonce()
	baseURL := getOIDCBaseURL()
	loginURL, err := svc.LoginURL(ctx, baseURL, nonce)
	if err != nil {
		c.JSON(consts.StatusBadGateway, map[string]interface{}{"code": errcode.CodeUnavailable, "message": err.Error()})
		return
	}
	setOIDCNonceCookie(c, nonce)
	c.Header("Cache-Control", "no-store")
	c.Redirect(consts.StatusFound, []byte(loginURL))
}

// OIDCCallback IdP 回调：换发本站 JWT 并 302 回前端（GET /api/v1/user/oidc/callback）
func OIDCCallback(ctx context.Context, c *app.RequestContext) {
	svc := getOIDCService()
	if svc == nil || !svc.Enabled() {
		c.JSON(consts.StatusNotFound, map[string]interface{}{"code": errcode.CodeNotFound, "message": "OIDC 登录未启用"})
		return
	}
	// nonce Cookie 一次性：无论成败回调结束即作废，旧 code+state 重放不再可用
	nonce := string(c.Cookie(oidcNonceCookie))
	defer clearOIDCNonceCookie(c)
	code := c.Query("code")
	state := c.Query("state")
	if code == "" {
		redirectOIDCError(c, "授权失败：缺少 code")
		return
	}
	token, err := svc.ExchangeCallback(ctx, getOIDCBaseURL(), code, state, nonce)
	if err != nil {
		redirectOIDCError(c, err.Error())
		return
	}
	// HttpOnly 会话 Cookie 承载会话；回跳不再携带 token（2026-10-05 遗留修复：
	// 令牌不入浏览器历史/剪贴板同步）
	middleware.SetSessionCookie(c, token, int(auth.SessionExpiry().Seconds()))
	c.Header("Cache-Control", "no-store")
	c.Redirect(consts.StatusFound, []byte(svc.FrontendCallback()))
}
