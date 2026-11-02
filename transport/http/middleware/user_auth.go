package middleware

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/pigeonbox/core/pkg/auth"
	pkgmw "github.com/pigeonbox/core/pkg/middleware"
)

const (
	// UserRoleAdmin 管理员角色
	UserRoleAdmin = "admin"
	// UserRoleUser 普通用户角色
	UserRoleUser = "user"
)

// isRevoked 检查 token 是否已被注销（logout 黑名单；与 pkg/middleware 同一存储）
func isRevoked(ctx context.Context, token string) bool {
	return auth.IsTokenRevoked(ctx, token)
}

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
func UserAuth() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		tokenString, viaCookie := pkgmw.SessionToken(c)
		if tokenString == "" {
			respondUnauthorized(c, "未认证（缺少 Bearer 头或会话 Cookie）")
			return
		}

		claims, err := auth.ParseToken(tokenString)
		if err != nil {
			respondUnauthorized(c, "Invalid or expired token")
			return
		}

		// 注销黑名单检查（logout 端点写入；此前 /api/v1 组未接黑名单，
		// 登出后 token 仍然可用）
		if isRevoked(ctx, tokenString) {
			respondUnauthorized(c, "Token has been revoked")
			return
		}

		// CSRF 门禁（仅 Cookie 认证的非安全方法；与 pkg AuthMiddleware 同序）
		if !pkgmw.CSRFAllowed(c, viaCookie) {
			respondForbidden(c, "缺少 CSRF 头（Cookie 认证的写请求须携带 X-Requested-With: XMLHttpRequest）")
			return
		}

		// 身份复核（2026-10-08 加固）：与 pkg AuthMiddleware 共享 30s 缓存回查——
		// 此前本中间件只验签名+黑名单，封禁/降权/改密后的旧 token 在
		// /api/v1/user/requests* 上仍可用到自然过期
		if !pkgmw.IdentityFresh(ctx, claims) {
			respondUnauthorized(c, "Session is no longer valid")
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

		if GetUserRole(c) != role {
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

		currentRole := GetUserRole(c)
		for _, role := range roles {
			if currentRole == role {
				c.Next(ctx)
				return
			}
		}

		respondForbidden(c, "Access denied: insufficient permissions")
	}
}
