package middleware

// L8 · authz：/api/v1 用户面的角色闸门（RequireAdmin / RequireRole / RequireAnyRole）。
//
// 威胁：这三个是「纵向越权」的唯一执行点——它们读上下文里的身份与角色做判定。
// 此前三个函数全部 0 覆盖（transport/http/middleware 包整体只有 44% 覆盖），
// 于是：
//   - 判定顺序被改（先判角色后判认证 ⇒ 未登录拿到 403 而不是 401）无人发现；
//   - 角色比较被改成"包含 admin"（"superadmin" 命中）无人发现；
//   - RequireAnyRole 传空角色列表时被改成"放行"无人发现。
//
// 判据：未认证 ⇒ 401；已认证但角色不足 ⇒ 403 且业务 handler 不执行；
// 角色满足 ⇒ 放行。

import (
	"context"
	"net/http"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/test/assert"
	"github.com/cloudwego/hertz/pkg/common/ut"
)

// l8Identity 注入一个已认证身份（身份与角色由前置中间件写入上下文）。
func l8Identity(userID uint, role string) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		if userID > 0 {
			c.Set(ContextKeyUserID, userID)
		}
		c.Set(ContextKeyUserRole, role)
		c.Next(ctx)
	}
}

// l8Serve 起一个只挂目标闸门的路由，返回 (状态码, 业务 handler 是否被执行)。
func l8Serve(t *testing.T, identity app.HandlerFunc, gate app.HandlerFunc) (int, bool) {
	t.Helper()
	ran := false
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.GET("/t", identity, gate, func(ctx context.Context, c *app.RequestContext) {
		ran = true
		c.JSON(http.StatusOK, map[string]int{"code": 0})
	})
	w := ut.PerformRequest(h.Engine, http.MethodGet, "/t", nil)
	return w.Result().StatusCode(), ran
}

// l8AssertNotRan 业务 handler 不得被执行（越权面必须止步在闸门之前）。
func l8AssertNotRan(t *testing.T, ran bool, msg string) {
	t.Helper()
	if ran {
		t.Fatal(msg)
	}
}

// TestL8_RequireAdmin 三态：未认证 401 / 普通用户 403 / 管理员放行。
func TestL8_RequireAdmin(t *testing.T) {
	// 1) 未认证 ⇒ 401
	code, ran := l8Serve(t, l8Identity(0, ""), RequireAdmin())
	assert.DeepEqual(t, http.StatusUnauthorized, code)
	l8AssertNotRan(t, ran, "未认证不得放行到业务 handler")

	// 2) 已认证但非管理员 ⇒ 403
	code, ran = l8Serve(t, l8Identity(3, "user"), RequireAdmin())
	assert.DeepEqual(t, http.StatusForbidden, code)
	l8AssertNotRan(t, ran, "非管理员不得放行到业务 handler")

	// 3) 管理员 ⇒ 放行
	code, ran = l8Serve(t, l8Identity(3, "admin"), RequireAdmin())
	assert.DeepEqual(t, http.StatusOK, code)
	assert.True(t, ran)
}

// TestL8_RequireAdminRejectsRolePrefix 普通角色不得因"含 admin"而命中。
//
// 判据钉住精确相等（pkg/authz.RoleSatisfies 的口径）。
func TestL8_RequireAdminRejectsRolePrefix(t *testing.T) {
	code, ran := l8Serve(t, l8Identity(3, "superadmin"), RequireAdmin())
	assert.DeepEqual(t, http.StatusForbidden, code)
	l8AssertNotRan(t, ran, "superadmin 不是 admin，不得放行")
}

// TestL8_RequireRole 指定角色：不足 ⇒ 403，满足 ⇒ 放行。
func TestL8_RequireRole(t *testing.T) {
	code, ran := l8Serve(t, l8Identity(3, "user"), RequireRole("admin"))
	assert.DeepEqual(t, http.StatusForbidden, code)
	l8AssertNotRan(t, ran, "角色不足不得放行")

	code, ran = l8Serve(t, l8Identity(3, "admin"), RequireRole("admin"))
	assert.DeepEqual(t, http.StatusOK, code)
	assert.True(t, ran)

	// 未认证 ⇒ 401（认证判定先于角色判定）
	code, ran = l8Serve(t, l8Identity(0, ""), RequireRole("admin"))
	assert.DeepEqual(t, http.StatusUnauthorized, code)
	l8AssertNotRan(t, ran, "未认证不得放行")
}

// TestL8_RequireAnyRole 任一角色：命中放行、未命中 403、空角色列表一律拒。
func TestL8_RequireAnyRole(t *testing.T) {
	code, ran := l8Serve(t, l8Identity(3, "ops"), RequireAnyRole("admin", "ops"))
	assert.DeepEqual(t, http.StatusOK, code)
	assert.True(t, ran)

	code, ran = l8Serve(t, l8Identity(3, "user"), RequireAnyRole("admin", "ops"))
	assert.DeepEqual(t, http.StatusForbidden, code)
	l8AssertNotRan(t, ran, "角色不足不得放行")

	// 空角色列表 ⇒ 无人满足 ⇒ 403（不得当成"无限制"）
	code, ran = l8Serve(t, l8Identity(3, "admin"), RequireAnyRole())
	assert.DeepEqual(t, http.StatusForbidden, code)
	l8AssertNotRan(t, ran, "空角色列表不得放行")

	// 未认证 ⇒ 401
	code, ran = l8Serve(t, l8Identity(0, ""), RequireAnyRole("admin"))
	assert.DeepEqual(t, http.StatusUnauthorized, code)
	l8AssertNotRan(t, ran, "未认证不得放行")
}

// TestL8_IsAdminUsesAuthzExecutionPoint IsAdmin 的角色判定必须与 pkg/authz 同源。
//
// 判据：IsAuthenticated 看 user_id>0；IsAdmin 对 "admin" 为真、对其它角色为假。
func TestL8_IsAdminUsesAuthzExecutionPoint(t *testing.T) {
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	var authed, admin bool
	h.GET("/t", l8Identity(3, "admin"), func(ctx context.Context, c *app.RequestContext) {
		authed = IsAuthenticated(c)
		admin = IsAdmin(c)
		c.JSON(http.StatusOK, map[string]int{"code": 0})
	})
	ut.PerformRequest(h.Engine, http.MethodGet, "/t", nil)
	assert.True(t, authed)
	assert.True(t, admin)

	h2 := server.New(server.WithHostPorts("127.0.0.1:0"))
	h2.GET("/t", l8Identity(3, "user"), func(ctx context.Context, c *app.RequestContext) {
		admin = IsAdmin(c)
		c.JSON(http.StatusOK, map[string]int{"code": 0})
	})
	ut.PerformRequest(h2.Engine, http.MethodGet, "/t", nil)
	assert.False(t, admin)
}
