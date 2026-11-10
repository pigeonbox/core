// OIDC SSO HTTP 端点（P2，手写路由）。
package handler

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/pigeonbox/contracts/errcode"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	oidcApp "github.com/pigeonbox/core/app/oidc"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/auth"
	"github.com/pigeonbox/core/pkg/middleware"
)

var oidcSvc *oidcApp.Service

// SetOIDCService 注入 OIDC 服务（bootstrap 调用）。
//
// 旧服务下线前零化其密钥持有器（core#81 热更路径：旧 secret 的混淆存储随之
// 清零，不留 freed-heap 明文）。⚠️ identity 守卫：新旧服务共享同一持有器时
// （未改 secret 的重建：conf/mirror/服务三方同指针）**绝不能清**——那会把
// 仍在服役的活密钥一并清掉。换密钥路径的旧持有器已被 conf overlay 先行清零，
// 此处再清是幂等兜底（Clear 对已清持有器无害）。
func SetOIDCService(s *oidcApp.Service) {
	if old := oidcSvc; old != nil && old != s {
		// s == nil（卸载/测试复位）视为「与任何旧持有器不同」：旧服务独占的
		// 密钥随之零化。同指针守卫照旧——共享持有器（未改 secret 的重建）不清。
		if s == nil || old.SecretHolder() != s.SecretHolder() {
			old.ClearSecret()
		}
	}
	oidcSvc = s
}

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

// oidcErrorRedirect OIDC 失败回跳的前端地址（hash 路由；query 另挂）。
const oidcErrorRedirect = "/#/user/login"

// oidcPublicErrorCode / oidcPublicErrorMsg OIDC 失败 → (短码, 可公开短文案)。
//
// ⚠️ **绝不透传 err.Error()**：那会泄露 IdP 内部细节（issuer / 端点 URL /
// token 响应片段 / 账号标识）⇒ 只回**受限映射**后的短文案 + 稳定短码。
// 短码是给前端/运维做**可区分**判定的（前端可按码选文案，运维可按码定位）。
func oidcPublicError(err error) (code, msg string) {
	switch {
	case err == nil:
		return "login_failed", "登录失败，请重试"
	case errors.Is(err, oidcApp.ErrAccountDisabled):
		return "account_disabled", "账号已被禁用"
	case errors.Is(err, oidcApp.ErrStateInvalid):
		return "state_invalid", "登录请求已过期，请重新登录"
	case errors.Is(err, oidcApp.ErrEmailInvalid):
		// core#119 同族：IdP email claim 格式非法 ⇒ 独立短码（受限映射，
		// 不透传原始 err——那会把 claim 原值回显进回跳 query）
		return "email_invalid", "身份提供方返回的邮箱格式无效"
	default:
		// 其余（IdP discovery 失败 / token 交换失败 / userinfo 失败 / 建号失败…）
		// 全部归到通用失败：不透传细节，也不区分（区分需要透传细节）。
		return "login_failed", "登录失败，请重试"
	}
}

// redirectOIDCError 302 回前端登录页并携带**可区分**的错误标识。
//
// ⚠️ 2026-10-26 审计修复（P2）：此前 msg 是**死参数**——注释声称「携带错误信息」，
// 实现却只回裸 `/#/user/login?oidc_error=1` ⇒ 用户与运维无法区分「IdP 挂了」
// 「state 过期」「账号被禁用」。
// ⇒ 回跳带上三个参数：
//   - oidc_error=1：**保持既有前端契约**（前端按该参数存在与否判失败）；
//   - oidc_code=<短码>：可区分的稳定标识（前端可按码选文案，运维可按码定位）；
//   - msg=<可公开短文案>：urlencode 后的短文案（供前端直接展示）。
func redirectOIDCError(c *app.RequestContext, code, msg string) {
	c.Header("Cache-Control", "no-store")
	q := url.Values{}
	q.Set("oidc_error", "1")
	q.Set("oidc_code", code)
	q.Set("msg", msg)
	c.Redirect(consts.StatusFound, []byte(oidcErrorRedirect+"?"+q.Encode()))
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
	// nonce 生成失败 ⇒ 拒绝登录（core#86：绝不下发弱 nonce——弱 nonce 会让
	// state 绑定的 login CSRF 防线失效；这是本地熵源故障，500/CodeInternal）
	nonce, err := oidcApp.GenerateNonce()
	if err != nil {
		c.JSON(consts.StatusInternalServerError, map[string]interface{}{"code": errcode.CodeInternal, "message": "OIDC 登录发起失败（随机源不可用）"})
		return
	}
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
		redirectOIDCError(c, "missing_code", "授权失败：缺少授权码")
		return
	}
	token, err := svc.ExchangeCallback(ctx, getOIDCBaseURL(), code, state, nonce)
	if err != nil {
		ecode, emsg := oidcPublicError(err)
		redirectOIDCError(c, ecode, emsg)
		return
	}
	// HttpOnly 会话 Cookie 承载会话；回跳不再携带 token（2026-10-05 遗留修复：
	// 令牌不入浏览器历史/剪贴板同步）
	middleware.SetSessionCookie(c, token, int(auth.SessionExpiry().Seconds()))
	c.Header("Cache-Control", "no-store")
	c.Redirect(consts.StatusFound, []byte(svc.FrontendCallback()))
}
