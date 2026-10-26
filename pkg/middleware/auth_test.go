package middleware

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/test/assert"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/pigeonbox/core/pkg/auth"
)

// AdminMiddleware 行为回归（2026-10-05 审计 P0）：
// 此前实现内联调用 AuthMiddleware()——其成功路径 c.Next 会把业务 handler 先
// 执行完，角色检查沦为"事后追认"：任意注册用户可执行全部 /admin/*（含建号/
// 改配置），读接口的业务响应体还会拼在 403 前面泄露。修复后必须：
//  1. 普通用户 token → 403，业务 handler 不执行（副作用为零）；
//  2. 伪造/无效 token → 401；
//  3. admin token → 放行，身份写入 RequestContext。
func TestAdminMiddlewareRoleGate(t *testing.T) {
	auth.SetJWTSecret("test-secret-for-admin-middleware-32chars")
	t.Cleanup(func() { auth.SetJWTSecret("") })

	userToken, err := auth.GenerateToken(2, "alice", "user")
	if err != nil {
		t.Fatal(err)
	}
	adminToken, err := auth.GenerateAdminToken(1, "admin")
	if err != nil {
		t.Fatal(err)
	}

	newServer := func(handlerRan *bool) *server.Hertz {
		h := server.New(server.WithHostPorts("127.0.0.1:0"))
		h.GET("/admin/secret", AdminMiddleware(), func(ctx context.Context, c *app.RequestContext) {
			*handlerRan = true
			c.JSON(http.StatusOK, map[string]string{"secret": "value"})
		})
		return h
	}

	// 1. 普通用户：403 且 handler 不执行
	ran := false
	h := newServer(&ran)
	w := ut.PerformRequest(h.Engine, http.MethodGet, "/admin/secret", nil,
		ut.Header{Key: "Authorization", Value: "Bearer " + userToken})
	resp := w.Result()
	assert.DeepEqual(t, http.StatusForbidden, resp.StatusCode())
	if ran {
		t.Fatal("角色检查前业务 handler 已被执行（P0 回归）")
	}
	if strings.Contains(string(resp.Body()), "secret") {
		t.Fatal("403 响应不得包含业务数据")
	}

	// 2. 无效 token：401
	ran = false
	h = newServer(&ran)
	w = ut.PerformRequest(h.Engine, http.MethodGet, "/admin/secret", nil,
		ut.Header{Key: "Authorization", Value: "Bearer not-a-jwt"})
	assert.DeepEqual(t, http.StatusUnauthorized, w.Result().StatusCode())
	if ran {
		t.Fatal("无效 token 下业务 handler 被执行")
	}

	// 3. admin token：200 且身份注入
	ran = false
	h = newServer(&ran)
	w = ut.PerformRequest(h.Engine, http.MethodGet, "/admin/secret", nil,
		ut.Header{Key: "Authorization", Value: "Bearer " + adminToken})
	assert.DeepEqual(t, http.StatusOK, w.Result().StatusCode())
	if !ran {
		t.Fatal("admin token 未放行业务 handler")
	}
	assert.DeepEqual(t, "admin", string(w.Result().Header.Get("X-Role")))
}

// OptionalAuthMiddleware 吊销语义回归（2026-10-05 审计 P1）：已注销 token 在
// 可选身份路由必须降级为匿名，不得继续注入 user_id。
func TestOptionalAuthMiddlewareRespectsBlacklist(t *testing.T) {
	auth.SetJWTSecret("test-secret-for-optional-auth-32ch")
	t.Cleanup(func() { auth.SetJWTSecret("") })

	token, err := auth.GenerateToken(3, "bob", "user")
	if err != nil {
		t.Fatal(err)
	}

	// 纯内存黑名单模式：注销后该 token 应视为匿名
	auth.SetBlacklistRedis(nil)
	auth.RevokeToken(context.Background(), token, 1<<20)

	ran := false
	var handler app.HandlerFunc = func(ctx context.Context, c *app.RequestContext) {
		ran = true
		uid, ok := UserIDFromContext(ctx)
		if ok || uid != 0 {
			t.Errorf("已注销 token 不应注入身份, got uid=%d", uid)
		}
		if _, exists := c.Get("user_id"); exists {
			t.Error("已注销 token 不应写入 c.Set(user_id)")
		}
		c.JSON(http.StatusOK, map[string]string{})
	}
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.POST("/share/text", append(OptionalIdentity(), handler)...)

	w := ut.PerformRequest(h.Engine, http.MethodPost, "/share/text", nil,
		ut.Header{Key: "Authorization", Value: "Bearer " + token})
	assert.DeepEqual(t, http.StatusOK, w.Result().StatusCode())
	if !ran {
		t.Fatal("匿名语义应放行 handler")
	}
}

// 身份复核回归（2026-10-05 审计 P2）：封禁/降权/会话纪元过期即时生效。
func TestIdentityFreshEnforcement(t *testing.T) {
	// 身份缓存是包级全局 ⇒ 本用例必须用专属缓存，否则会吃到别的用例的残留
	// （`go test -count=2` 必红，见 useFreshIdentityCache 的注释）。
	useFreshIdentityCache(t, nil)
	auth.SetJWTSecret("test-secret-for-identity-fresh-32ch")
	t.Cleanup(func() { auth.SetJWTSecret("") })

	users := map[uint]*IdentityRecord{
		7: {Status: "active", Role: "user", Epoch: 0},
	}
	SetIdentityLoader(func(ctx context.Context, userID uint) (*IdentityRecord, error) {
		if rec, ok := users[userID]; ok {
			return rec, nil
		}
		return nil, fmt.Errorf("user not found")
	})
	t.Cleanup(func() { SetIdentityLoader(nil) })
	auth.SetEpochLoader(func(userID uint) int { return users[userID].Epoch })
	t.Cleanup(func() { auth.SetEpochLoader(nil) })

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.GET("/me", AuthMiddleware(), func(ctx context.Context, c *app.RequestContext) {
		c.JSON(http.StatusOK, map[string]string{})
	})

	token0, err := auth.GenerateToken(7, "carol", "user")
	if err != nil {
		t.Fatal(err)
	}
	assert.DeepEqual(t, http.StatusOK, ut.PerformRequest(h.Engine, http.MethodGet, "/me", nil,
		ut.Header{Key: "Authorization", Value: "Bearer " + token0}).Result().StatusCode())

	// 封禁 → 立即 401
	users[7].Status = "banned"
	InvalidateIdentity(7)
	assert.DeepEqual(t, http.StatusUnauthorized, ut.PerformRequest(h.Engine, http.MethodGet, "/me", nil,
		ut.Header{Key: "Authorization", Value: "Bearer " + token0}).Result().StatusCode())

	// 解封但纪元 +1（模拟改密/降权）→ 旧 token 仍 401
	users[7] = &IdentityRecord{Status: "active", Role: "user", Epoch: 1}
	InvalidateIdentity(7)
	assert.DeepEqual(t, http.StatusUnauthorized, ut.PerformRequest(h.Engine, http.MethodGet, "/me", nil,
		ut.Header{Key: "Authorization", Value: "Bearer " + token0}).Result().StatusCode())

	// 新签发 token 携带新纪元 → 放行
	token1, err := auth.GenerateToken(7, "carol", "user")
	if err != nil {
		t.Fatal(err)
	}
	assert.DeepEqual(t, http.StatusOK, ut.PerformRequest(h.Engine, http.MethodGet, "/me", nil,
		ut.Header{Key: "Authorization", Value: "Bearer " + token1}).Result().StatusCode())

	// DB 角色回填：claim 声称 admin 但 DB 是 user → AdminMiddleware 403
	users[7].Role = "user"
	adminClaimToken, err := auth.GenerateToken(7, "carol", "admin") // 伪造场景：泄露的旧 admin claim
	if err != nil {
		t.Fatal(err)
	}
	h2 := server.New(server.WithHostPorts("127.0.0.1:0"))
	h2.GET("/admin/x", AdminMiddleware(), func(ctx context.Context, c *app.RequestContext) {
		c.JSON(http.StatusOK, map[string]string{})
	})
	assert.DeepEqual(t, http.StatusForbidden, ut.PerformRequest(h2.Engine, http.MethodGet, "/admin/x", nil,
		ut.Header{Key: "Authorization", Value: "Bearer " + adminClaimToken}).Result().StatusCode())
}

// 会话 Cookie 认证回归（2026-10-05 遗留修复）：Cookie 可认证；Cookie 认证的
// 写请求须带 CSRF 头（X-Requested-With）；Bearer 认证不受 CSRF 门禁影响。
func TestSessionCookieAuthAndCSRF(t *testing.T) {
	auth.SetJWTSecret("test-secret-for-cookie-auth-32char")
	t.Cleanup(func() { auth.SetJWTSecret("") })

	token, err := auth.GenerateToken(9, "dave", "user")
	if err != nil {
		t.Fatal(err)
	}

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	ok := func(ctx context.Context, c *app.RequestContext) {
		c.JSON(http.StatusOK, map[string]string{})
	}
	h.GET("/me", AuthMiddleware(), ok)
	h.POST("/act", AuthMiddleware(), ok)

	// Cookie GET：放行
	w := ut.PerformRequest(h.Engine, http.MethodGet, "/me", nil,
		ut.Header{Key: "Cookie", Value: SessionCookieName + "=" + token})
	assert.DeepEqual(t, http.StatusOK, w.Result().StatusCode())

	// Cookie POST 无 CSRF 头：403
	w = ut.PerformRequest(h.Engine, http.MethodPost, "/act", nil,
		ut.Header{Key: "Cookie", Value: SessionCookieName + "=" + token})
	assert.DeepEqual(t, http.StatusForbidden, w.Result().StatusCode())

	// Cookie POST 带正确 CSRF 头：200
	w = ut.PerformRequest(h.Engine, http.MethodPost, "/act", nil,
		ut.Header{Key: "Cookie", Value: SessionCookieName + "=" + token},
		ut.Header{Key: CSRFHeaderName, Value: CSRFHeaderValue})
	assert.DeepEqual(t, http.StatusOK, w.Result().StatusCode())

	// Cookie POST 带错误值：403
	w = ut.PerformRequest(h.Engine, http.MethodPost, "/act", nil,
		ut.Header{Key: "Cookie", Value: SessionCookieName + "=" + token},
		ut.Header{Key: CSRFHeaderName, Value: "evil"})
	assert.DeepEqual(t, http.StatusForbidden, w.Result().StatusCode())

	// Bearer POST 无 CSRF 头：200（门禁只针对 Cookie 通道）
	w = ut.PerformRequest(h.Engine, http.MethodPost, "/act", nil,
		ut.Header{Key: "Authorization", Value: "Bearer " + token})
	assert.DeepEqual(t, http.StatusOK, w.Result().StatusCode())

	// 无身份 GET：401
	w = ut.PerformRequest(h.Engine, http.MethodGet, "/me", nil)
	assert.DeepEqual(t, http.StatusUnauthorized, w.Result().StatusCode())
}
