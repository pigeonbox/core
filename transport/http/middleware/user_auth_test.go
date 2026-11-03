package middleware

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/test/assert"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/pigeonbox/core/pkg/auth"
)

// UserAuth 双通道回归锁（2026-11-02 真机缺陷）：
// 浏览器登录态是 HttpOnly 会话 Cookie（pb_token），其余接口由 pkg AuthMiddleware
// 双通道放行，而 /api/v1/user/requests* 绑的 UserAuth 只认 Authorization: Bearer 头
// ⇒ 浏览器访问「我的请求/寄件码」恒 401「Authorization header is required」，
// 前端误报「未登录或登录已过期」（refresh 重放仍是 Cookie，救不回来）。
// 修复后必须：
//  1. 仅 Cookie 的会话请求放行（本文件即转红判据——旧实现下恒 401）；
//  2. Bearer 头行为不变（脚本/CLI/MCP 消费方零影响）；
//  3. Cookie 认证的非安全方法必须过 CSRF 头门禁（X-Requested-With），
//     Bearer 认证不受此限；
//  4. 语义保持「只认用户 JWT」：无凭据 / 非 JWT Bearer（如 API Key）仍拒。
func TestUserAuthCookieSessionAccepted(t *testing.T) {
	auth.SetJWTSecret("test-secret-for-userauth-cookie-32chars!")
	t.Cleanup(func() { auth.SetJWTSecret("") })

	token, err := auth.GenerateToken(3, "alice", "user")
	if err != nil {
		t.Fatal(err)
	}

	var gotUserID uint
	var gotUsername, gotRole, gotAuthType string
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.GET("/api/v1/user/requests", UserAuth(), func(ctx context.Context, c *app.RequestContext) {
		gotUserID = GetUserID(c)
		gotUsername = GetUsername(c)
		gotRole = GetUserRole(c)
		gotAuthType = GetAuthType(c)
		c.JSON(http.StatusOK, map[string]int{"code": 0})
	})

	w := ut.PerformRequest(h.Engine, http.MethodGet, "/api/v1/user/requests", nil,
		ut.Header{Key: "Cookie", Value: "pb_token=" + token},
		ut.Header{Key: "X-Requested-With", Value: "XMLHttpRequest"})
	resp := w.Result()
	assert.DeepEqual(t, http.StatusOK, resp.StatusCode())
	assert.DeepEqual(t, uint(3), gotUserID)
	assert.DeepEqual(t, "alice", gotUsername)
	assert.DeepEqual(t, "user", gotRole)
	assert.DeepEqual(t, "jwt", gotAuthType)
}

func TestUserAuthBearerStillWorks(t *testing.T) {
	auth.SetJWTSecret("test-secret-for-userauth-cookie-32chars!")
	t.Cleanup(func() { auth.SetJWTSecret("") })

	token, err := auth.GenerateToken(7, "bob", "admin")
	if err != nil {
		t.Fatal(err)
	}

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.GET("/t", UserAuth(), func(ctx context.Context, c *app.RequestContext) {
		c.JSON(http.StatusOK, map[string]int{"code": 0})
	})

	// Bearer 头（不带任何 Cookie）必须照常放行
	w := ut.PerformRequest(h.Engine, http.MethodGet, "/t", nil,
		ut.Header{Key: "Authorization", Value: "Bearer " + token})
	assert.DeepEqual(t, http.StatusOK, w.Result().StatusCode())
}

func TestUserAuthCookieWriteRequiresCSRFHeader(t *testing.T) {
	auth.SetJWTSecret("test-secret-for-userauth-cookie-32chars!")
	t.Cleanup(func() { auth.SetJWTSecret("") })

	token, err := auth.GenerateToken(3, "alice", "user")
	if err != nil {
		t.Fatal(err)
	}

	ran := false
	newServer := func() *server.Hertz {
		ran = false
		h := server.New(server.WithHostPorts("127.0.0.1:0"))
		h.POST("/t", UserAuth(), func(ctx context.Context, c *app.RequestContext) {
			ran = true
			c.JSON(http.StatusOK, map[string]int{"code": 0})
		})
		return h
	}

	// 1. Cookie + 写方法 + 无 CSRF 头 ⇒ 403 且业务 handler 不执行（CSRF 面收口）
	h := newServer()
	w := ut.PerformRequest(h.Engine, http.MethodPost, "/t", nil,
		ut.Header{Key: "Cookie", Value: "pb_token=" + token})
	assert.DeepEqual(t, http.StatusForbidden, w.Result().StatusCode())
	if ran {
		t.Fatal("CSRF 门禁前业务 handler 已被执行")
	}

	// 2. Cookie + 写方法 + X-Requested-With ⇒ 放行
	h = newServer()
	w = ut.PerformRequest(h.Engine, http.MethodPost, "/t", nil,
		ut.Header{Key: "Cookie", Value: "pb_token=" + token},
		ut.Header{Key: "X-Requested-With", Value: "XMLHttpRequest"})
	assert.DeepEqual(t, http.StatusOK, w.Result().StatusCode())

	// 3. Bearer + 写方法 + 无 CSRF 头 ⇒ 不受限（与 pkg AuthMiddleware 同语义）
	h = newServer()
	w = ut.PerformRequest(h.Engine, http.MethodPost, "/t", nil,
		ut.Header{Key: "Authorization", Value: "Bearer " + token})
	assert.DeepEqual(t, http.StatusOK, w.Result().StatusCode())
}

func TestUserAuthRejectsMissingAndNonJWT(t *testing.T) {
	auth.SetJWTSecret("test-secret-for-userauth-cookie-32chars!")
	t.Cleanup(func() { auth.SetJWTSecret("") })

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.GET("/t", UserAuth(), func(ctx context.Context, c *app.RequestContext) {
		c.JSON(http.StatusOK, map[string]int{"code": 0})
	})

	// 1. 无凭据 ⇒ 401
	w := ut.PerformRequest(h.Engine, http.MethodGet, "/t", nil)
	resp := w.Result()
	assert.DeepEqual(t, http.StatusUnauthorized, resp.StatusCode())

	// 2. 非 JWT 的 Bearer（API Key 等）⇒ 401「Invalid or expired token」
	//    （UserAuth 只认用户 JWT：Key 不能操作寄件码的语义不因本修复改变）
	w = ut.PerformRequest(h.Engine, http.MethodGet, "/t", nil,
		ut.Header{Key: "Authorization", Value: "Bearer pk_not_a_jwt"})
	resp = w.Result()
	assert.DeepEqual(t, http.StatusUnauthorized, resp.StatusCode())
	if !strings.Contains(string(resp.Body()), "Invalid or expired token") {
		t.Fatalf("非 JWT Bearer 应报 Invalid or expired token，实际: %s", resp.Body())
	}

	// 3. 伪造 Cookie ⇒ 401
	w = ut.PerformRequest(h.Engine, http.MethodGet, "/t", nil,
		ut.Header{Key: "Cookie", Value: "pb_token=forged"})
	assert.DeepEqual(t, http.StatusUnauthorized, w.Result().StatusCode())
}
