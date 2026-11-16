package middleware

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/pigeonbox/core/pkg/authz"
	pkgmw "github.com/pigeonbox/core/pkg/middleware"
)

const (
	// UserRoleAdmin 管理员角色
	UserRoleAdmin = "admin"
	// UserRoleUser 普通用户角色
	UserRoleUser = "user"
)

// UserAuth 用户 JWT 认证中间件
// 验证用户会话 JWT 并将用户信息注入到上下文。
//
// 令牌来源与 pkg AuthMiddleware 同一双通道：Authorization: Bearer 头优先，
// 回退会话 Cookie（pb_token，浏览器端默认）。2026-11-02 修复：此前只认
// Bearer 头，浏览器 Cookie 会话在 /api/v1/user/requests* 上恒 401「Authorization
// header is required」——前端 HttpOnly 会话全部走 Cookie（其余接口由
// AuthMiddleware 双通道放行），唯独本中间件漏了 Cookie 通道，前端自动 refresh
// 后重放仍是 Cookie ⇒ 用户被误踢到登录提示。Cookie 认证的非安全方法另有
// CSRF 头门禁（session.go）。语义不变：只认用户 JWT，API Key 不可操作寄件码。
//
// 判定链经 pkg/middleware.EvaluateJWT 单一执行点（core#173 第四片收口，
// 与 AuthMiddleware 同一检查序）；本函数只保留**本入口的两处方言**：
//   - 身份回查失败文案 "Session is no longer valid"（pkg 面是「账号状态已变更」）；
//   - 注入只做 c.Set 四件套，不写 ctx 身份/回显头（与 pkg 栈的有意差异，
//     统一前需验证 /api/v1 组审计归因的预期行为）。
func UserAuth() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		tokenString, viaCookie := pkgmw.SessionToken(c)
		claims, rej := pkgmw.EvaluateJWT(ctx, c, tokenString, viaCookie, pkgmw.PolicySession)
		if rej != pkgmw.JWTAccepted {
			switch rej {
			case pkgmw.JWTCSRFRejected:
				respondForbidden(c, "缺少 CSRF 头（Cookie 认证的写请求须携带 X-Requested-With: XMLHttpRequest）")
			case pkgmw.JWTIdentityStale:
				respondUnauthorized(c, "Session is no longer valid")
			default:
				respondUnauthorized(c, pkgmw.RejectionMessage(rej))
			}
			return
		}
		pkgmw.SetJWTClaims(c, claims)

		c.Next(ctx)
	}
}

// RequireAdmin 要求管理员权限的中间件
// 必须配合 UserAuth 或其他认证中间件使用
func RequireAdmin() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		if !IsAuthenticated(c) {
			respondUnauthorized(c, "Authentication required")
			return
		}

		if !IsAdmin(c) {
			respondForbidden(c, "Admin access required")
			return
		}

		c.Next(ctx)
	}
}

// RequireRole 要求特定角色的中间件
// 必须配合 UserAuth 或其他认证中间件使用
func RequireRole(role string) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		if !IsAuthenticated(c) {
			respondUnauthorized(c, "Authentication required")
			return
		}

		if !authz.RoleSatisfies(GetUserRole(c), role) {
			respondForbidden(c, "Required role: "+role)
			return
		}

		c.Next(ctx)
	}
}

// RequireAnyRole 要求具备任一指定角色的中间件
// 必须配合 UserAuth 或其他认证中间件使用
func RequireAnyRole(roles ...string) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		if !IsAuthenticated(c) {
			respondUnauthorized(c, "Authentication required")
			return
		}

		if !authz.AnyRoleSatisfies(GetUserRole(c), roles...) {
			respondForbidden(c, "Access denied: insufficient permissions")
			return
		}

		c.Next(ctx)
	}
}
