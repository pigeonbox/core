// 会话自省端点（2026-10-26 自 bootstrap/routes_custom.go 的内联闭包提为导出函数）。
//
// ## 为什么提出来
//
// 这三条路由（/api/v1/user/refresh、/logout、/check-auth）原先是
// bootstrap/routes_custom.go 里的**内联闭包** ⇒ 它们既没有出现在 IDL 里，
// 也没有可被 gen/handler 复用的入口。
//
// 2026-10-26 的 IDL 化收编把这三条路由搬进了 contracts IDL（common.thrift ⇒
// gencommon 组 ⇒ public 与 admin **两面都注册**，与收敛前的无条件注册一致），
// 路由改由 gen/router 生成 ⇒ hz 生成的 handler 需要调用到业务逻辑 ⇒
// 把闭包体提为这里的导出函数，由 gen/handler/common 委托（gen/handler/* →
// transport/http/handler 是仓内既有的依赖方向，admin 域已有先例）。
//
// ⚠️ **逻辑与 wire 形状逐字照搬**（业务码 / 文案 / HTTP 状态码 / Cookie 行为）：
//
//	这三个端点被前端 401 拦截器与登出流程依赖，任何细微差异都会表现为
//	「刷新令牌失败 ⇒ 反复跳登录页」。
package handler

import (
	"context"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/pigeonbox/contracts/errcode"
	"github.com/pigeonbox/core/pkg/auth"
	"github.com/pigeonbox/core/pkg/middleware"
)

// RefreshSession 刷新会话令牌（POST /api/v1/user/refresh）。
//
// 令牌来源：Bearer 头或会话 Cookie；轮换同时下发新 Cookie（2026-10-05 遗留修复：
// 浏览器会话迁 HttpOnly Cookie）。Cookie 认证时须带 CSRF 头。
func RefreshSession(ctx context.Context, c *app.RequestContext) {
	oldToken, viaCookie := middleware.SessionToken(c)
	if oldToken == "" {
		c.JSON(consts.StatusUnauthorized, map[string]interface{}{"code": errcode.CodeUnauthorized, "message": "missing token"})
		return
	}
	if !middleware.CSRFAllowed(c, viaCookie) {
		c.JSON(consts.StatusForbidden, map[string]interface{}{"code": errcode.CodeForbidden, "message": "缺少 CSRF 头"})
		return
	}
	// core#106：续签前按 identityFresh 同款口径复核（账号 active + 纪元一致 +
	// 带 sid 的令牌回查会话存活）。此前 refresh 只验签名/过期/黑名单 ⇒ 改密、
	// 找回重置、退出所有设备、管理员重置/封禁（纪元 +1）的止损全部可被
	//「旧令牌换新令牌」绕过。复核失败回 401（与刷新失败同形），前端 401
	// 拦截器照常跳登录。
	if claims, perr := auth.ParseToken(oldToken); perr != nil || !middleware.IdentityFresh(ctx, claims) {
		middleware.ClearSessionCookie(c)
		c.JSON(consts.StatusUnauthorized, map[string]interface{}{"code": errcode.CodeUnauthorized, "message": "账号状态已变更，请重新登录"})
		return
	}
	newToken, err := auth.RefreshToken(ctx, oldToken)
	if err != nil {
		middleware.ClearSessionCookie(c)
		c.JSON(consts.StatusUnauthorized, map[string]interface{}{"code": errcode.CodeUnauthorized, "message": "token invalid or expired"})
		return
	}
	middleware.SetSessionCookie(c, newToken, int(auth.SessionExpiry().Seconds()))
	c.JSON(consts.StatusOK, map[string]interface{}{
		"code": 0, "message": "ok",
		"data": map[string]string{"token": newToken},
	})
}

// LogoutSession 显式注销（POST /api/v1/user/logout）。
//
// 令牌来源：Bearer 头或会话 Cookie；同时清除 Cookie。
// 黑名单 TTL = token 剩余有效期（过期后自然失效，无需清理任务）。
func LogoutSession(ctx context.Context, c *app.RequestContext) {
	token, _ := middleware.SessionToken(c)
	if token == "" {
		c.JSON(consts.StatusUnauthorized, map[string]interface{}{"code": errcode.CodeUnauthorized, "message": "missing token"})
		return
	}
	if claims, err := auth.ParseToken(token); err == nil {
		remaining := time.Until(claims.ExpiresAt.Time)
		auth.RevokeToken(ctx, token, remaining)
	}
	middleware.ClearSessionCookie(c)
	c.JSON(consts.StatusOK, map[string]interface{}{"code": 0, "message": "已退出登录"})
}

// CheckAuth 校验登录态并回带用户信息（GET /api/v1/user/check-auth）。
//
// user_id / username / role 由中间件写入上下文。
func CheckAuth(ctx context.Context, c *app.RequestContext) {
	uid, _ := c.Get("user_id")
	username, _ := c.Get("username")
	role, _ := c.Get("role")
	c.JSON(consts.StatusOK, map[string]interface{}{
		"code":    0,
		"message": "ok",
		"data": map[string]interface{}{
			"id":       uid,
			"username": username,
			"role":     role,
		},
	})
}
