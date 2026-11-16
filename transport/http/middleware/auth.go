package middleware

import (
	"net/http"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/pigeonbox/contracts/errcode"
	"github.com/pigeonbox/core/pkg/authz"
	"github.com/pigeonbox/core/pkg/errors"
)

const (
	// ContextKeyUserID 用户ID上下文键
	ContextKeyUserID = "user_id"
	// ContextKeyUsername 用户名上下文键
	ContextKeyUsername = "username"
	// ContextKeyUserRole 用户角色上下文键
	ContextKeyUserRole = "role"
	// ContextKeyAPIKeyID API Key ID上下文键
	ContextKeyAPIKeyID = "api_key_id"
	// ContextKeyAuthType 认证类型上下文键 (jwt/api_key)
	ContextKeyAuthType = "auth_type"
)

// GetUserID 从上下文获取用户ID
func GetUserID(c *app.RequestContext) uint {
	if userID, exists := c.Get(ContextKeyUserID); exists {
		if uid, ok := userID.(uint); ok {
			return uid
		}
	}
	return 0
}

// GetUsername 从上下文获取用户名
func GetUsername(c *app.RequestContext) string {
	if username, exists := c.Get(ContextKeyUsername); exists {
		if uname, ok := username.(string); ok {
			return uname
		}
	}
	return ""
}

// GetUserRole 从上下文获取用户角色
func GetUserRole(c *app.RequestContext) string {
	if role, exists := c.Get(ContextKeyUserRole); exists {
		if r, ok := role.(string); ok {
			return r
		}
	}
	return ""
}

// GetAPIKeyID 从上下文获取API Key ID
func GetAPIKeyID(c *app.RequestContext) uint {
	if keyID, exists := c.Get(ContextKeyAPIKeyID); exists {
		if id, ok := keyID.(uint); ok {
			return id
		}
	}
	return 0
}

// GetAuthType 从上下文获取认证类型
func GetAuthType(c *app.RequestContext) string {
	if authType, exists := c.Get(ContextKeyAuthType); exists {
		if t, ok := authType.(string); ok {
			return t
		}
	}
	return ""
}

// IsAuthenticated 检查是否已认证
func IsAuthenticated(c *app.RequestContext) bool {
	return GetUserID(c) > 0
}

// IsAdmin 检查是否是管理员（角色判定经 pkg/authz 单一执行点，core#173 第五片）
func IsAdmin(c *app.RequestContext) bool {
	return authz.IsAdminRole(GetUserRole(c))
}

// respondUnauthorized 返回401响应
func respondUnauthorized(c *app.RequestContext, message string) {
	c.Abort()
	c.JSON(http.StatusUnauthorized, map[string]interface{}{
		"code":    errors.CodeUnauthorized,
		"message": message,
	})
}

// respondForbidden 返回403响应
func respondForbidden(c *app.RequestContext, message string) {
	c.Abort()
	c.JSON(http.StatusForbidden, map[string]interface{}{
		"code":    errcode.CodeForbidden,
		"message": message,
	})
}
