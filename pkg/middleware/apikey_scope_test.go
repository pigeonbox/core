package middleware

// API Key 权限范围（scopes，2026-10-30）的行为断言。
//
// 守卫点（四个，缺一即失效）：
//  1. **缺省 / 空 = 全量** —— 与本机制引入前完全一致（老 Key 无需重建）；
//  2. 受限 Key 访问**超范围路径** ⇒ 403（且错误与「Key 无效」的 401 区分开）；
//  3. 受限 Key 访问**范围内路径** ⇒ 放行；
//  4. 范围外的路径（如 /openapi.json、/api/config）⇒ 不受范围限制
//     （否则「只给寄件权限」的 Key 连公开配置都读不到 ⇒ 范围失去意义）。

import (
	"context"
	"net/http"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/stretchr/testify/assert"
)

// performProbeAt 在**指定路径**上跑中间件链（既有 performProbe 固定注册 /probe，
// 测不了「不同路径 ⇒ 不同所需范围」这件事）。
func performProbeAt(t *testing.T, mws []app.HandlerFunc, path string, headers ...ut.Header) (int, string) {
	t.Helper()
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	handler := func(ctx context.Context, c *app.RequestContext) {
		c.JSON(http.StatusOK, map[string]string{"ok": "1"})
	}
	args := append([]app.HandlerFunc{}, mws...)
	args = append(args, handler)
	h.GET(path, args...)
	w := ut.PerformRequest(h.Engine, "GET", path, nil, headers...)
	return w.Code, w.Body.String()
}

// TestRequiredAPIKeyScope_路径映射 逐条核对路径→所需范围。
func TestRequiredAPIKeyScope_PathMapping(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/admin/config", ScopeAdmin},
		{"/api/v1/admin/anything", ScopeAdmin},
		{"/user/api-keys", ScopeUser},
		{"/api/v1/user/shares", ScopeUser},
		{"/share/file/", ScopeShare},
		{"/api/v1/share/multi-direct", ScopeShare},
		{"/chunk/upload/init/", ScopeShare},
		{"/anonymous/generate", ScopeShare},
		{"/api/v1/presign/upload", ScopeShare},
		{"/api/v1/request/abc/upload", ScopeShare},
		{"/request/abc", ScopeShare},
		// 不在范围表里 ⇒ 不要求任何能力
		{"/api/config", ""},
		{"/openapi.json", ""},
		{"/version", ""},
		{"/live", ""},
		{"/share-anything", ""}, // 前缀带结尾斜杠 ⇒ 不误伤
	}
	for _, c := range cases {
		assert.Equal(t, c.want, RequiredAPIKeyScope(c.path), "路径 %s 的所需范围", c.path)
	}
}

// TestAPIKeyScopeAllowed_空即全量 空范围 = 全量（老 Key 零影响）。
func TestAPIKeyScopeAllowed_EmptyMeansFull(t *testing.T) {
	assert.True(t, APIKeyScopeAllowed(nil, ScopeAdmin), "空范围必须放行管理面（老 Key 兼容）")
	assert.True(t, APIKeyScopeAllowed([]string{}, ScopeShare), "空范围必须放行")
	assert.True(t, APIKeyScopeAllowed(nil, ""), "不要求能力的路径恒放行")
}

// TestAPIKeyScopeAllowed_按范围判定 受限 Key 只放行自己范围内的能力。
func TestAPIKeyScopeAllowed_ByScope(t *testing.T) {
	scopes := []string{ScopeShare}
	assert.True(t, APIKeyScopeAllowed(scopes, ScopeShare))
	assert.False(t, APIKeyScopeAllowed(scopes, ScopeAdmin), "只给寄件权限的 Key 不该碰管理面")
	assert.False(t, APIKeyScopeAllowed(scopes, ScopeUser), "只给寄件权限的 Key 不该碰用户自有资源")
}

// TestValidateAPIKeyScopes 非法范围必须报错（不静默放宽）。
func TestValidateAPIKeyScopes(t *testing.T) {
	ok, err := ValidateAPIKeyScopes([]string{ScopeAdmin, ScopeShare, ScopeAdmin})
	assert.NoError(t, err)
	assert.Equal(t, []string{ScopeAdmin, ScopeShare}, ok, "去重且保序")

	empty, err := ValidateAPIKeyScopes(nil)
	assert.NoError(t, err)
	assert.Empty(t, empty, "缺省 = 全量（空切片）")

	_, err = ValidateAPIKeyScopes([]string{"root"})
	assert.Error(t, err, "非法范围必须报错（否则用户以为签发的是受限 Key，实际拿到全量）")
}

// TestScopeError_403 而非401 能力不足是 403（Key 本身有效）。
func TestScopeError_StatusIs403(t *testing.T) {
	c := app.NewContext(0)
	c.Request.SetMethod(http.MethodGet)
	c.Request.URI().SetPath("/admin/config")

	respondAPIKeyError(c, &ScopeError{Scope: ScopeAdmin})
	assert.Equal(t, http.StatusForbidden, c.Response.StatusCode(), "能力不足必须是 403")
}
