package middleware

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolveClientIP 可信代理解析语义全场景（含伪造对照）
func TestResolveClientIP(t *testing.T) {
	require.NoError(t, SetTrustedProxies([]string{"10.0.0.0/8"}))
	t.Cleanup(func() { _ = SetTrustedProxies(nil) })

	t.Run("直连不可信+伪造XFF → 忽略代理头按直连", func(t *testing.T) {
		got := ResolveClientIP("203.0.113.9:55555", "1.2.3.4")
		assert.Equal(t, "203.0.113.9", got, "直连不在可信网段时伪造 XFF 必须被忽略")
	})
	t.Run("直连可信+XFF → 采信真实客户端", func(t *testing.T) {
		got := ResolveClientIP("10.0.0.2:443", "203.0.113.7, 10.0.0.2")
		assert.Equal(t, "203.0.113.7", got)
	})
	t.Run("直连可信+XFF全为可信代理 → 取最左端", func(t *testing.T) {
		got := ResolveClientIP("10.0.0.2:443", "10.0.0.3, 10.0.0.2")
		assert.Equal(t, "10.0.0.3", got)
	})
	t.Run("直连可信+无XFF → 回退直连地址", func(t *testing.T) {
		got := ResolveClientIP("10.0.0.2:443", "")
		assert.Equal(t, "10.0.0.2", got)
	})
	t.Run("XFF 候选值带端口 → 归一为裸 IP（同一客户端换端口不换桶）", func(t *testing.T) {
		got := ResolveClientIP("10.0.0.2:443", "203.0.113.7:54321")
		assert.Equal(t, "203.0.113.7", got)
	})
	t.Run("XFF 候选值非法 → 跳过该段，不进返回值", func(t *testing.T) {
		got := ResolveClientIP("10.0.0.2:443", "not-an-ip, 203.0.113.7, 10.0.0.2")
		assert.Equal(t, "203.0.113.7", got, "非法候选值不得进限流/锁定键")
	})
	t.Run("XFF 整条链都非法 → 回退直连地址", func(t *testing.T) {
		got := ResolveClientIP("10.0.0.2:443", "not-an-ip")
		assert.Equal(t, "10.0.0.2", got)
	})
	t.Run("空对端地址 → unknown", func(t *testing.T) {
		got := ResolveClientIP("", "1.2.3.4")
		assert.Equal(t, "unknown", got)
	})
	// ⚠️ 放在最后：这一例把可信代理清空，后续用例都受它影响。
	t.Run("未配置可信网段直连部署 → 一律按直连", func(t *testing.T) {
		_ = SetTrustedProxies(nil)
		got := ResolveClientIP("203.0.113.9:55555", "1.2.3.4")
		assert.Equal(t, "203.0.113.9", got)
	})
}
