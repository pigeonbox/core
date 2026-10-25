package middleware

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSetTrustedProxies 可信代理清单的生效与降级（经公开的客户端 IP 解析面验证
// ——采信判定收编上游 oekit/pkg/convert.ResolveClientIP 后，本仓不再自持判定器）。
func TestSetTrustedProxies(t *testing.T) {
	require.NoError(t, SetTrustedProxies([]string{"10.0.0.0/8", "172.16.5.4"}))
	// 可信网段内的对端 ⇒ 采信 XFF 里的客户端地址
	assert.Equal(t, "1.2.3.4", ResolveClientIP("10.1.2.3:5678", "1.2.3.4"))
	// 裸 IP 形态的可信代理同样生效（上游 TrustedProxySet 归一为单主机网段）
	assert.Equal(t, "1.2.3.4", ResolveClientIP("172.16.5.4:5678", "1.2.3.4"))
	// 可信网段外的对端 ⇒ 忽略代理头
	assert.Equal(t, "203.0.113.9", ResolveClientIP("203.0.113.9:443", "1.2.3.4"))
	// 对端不是合法 IP ⇒ 不信任代理头
	assert.Equal(t, "not-an-ip", ResolveClientIP("not-an-ip:443", "1.2.3.4"))
	// 空列表 = 不信任任何代理 ⇒ 忽略代理头，按直连地址
	require.NoError(t, SetTrustedProxies(nil))
	assert.Equal(t, "10.1.2.3", ResolveClientIP("10.1.2.3:5678", "1.2.3.4"))
	// 非法 CIDR 报错
	assert.Error(t, SetTrustedProxies([]string{"300.1.2.3/24"}))
	// 裸 IP 形态的 IPv6 也生效（归一为 /128，此前本地实现错归一为 /32）
	require.NoError(t, SetTrustedProxies([]string{"2001:db8::1"}))
	assert.Equal(t, "1.2.3.4", ResolveClientIP("[2001:db8::1]:5678", "1.2.3.4"))
	// 还原为空，避免影响其他用例
	_ = SetTrustedProxies(nil)
}

func TestLockoutLockAndReset(t *testing.T) {
	// 纯内存模式（无 Redis）
	l := NewLockout(nil)
	key := FormatLockKey("login", "1.2.3.4", "admin")

	// 默认阈值 10 次触发锁定
	for i := 0; i < 9; i++ {
		locked, _ := l.RecordFailure(t.Context(), key)
		assert.False(t, locked, "第 %d 次失败不应锁定", i+1)
		_, isLocked := l.CheckLocked(t.Context(), key)
		assert.False(t, isLocked)
	}
	locked, lockSeconds := l.RecordFailure(t.Context(), key)
	assert.True(t, locked, "第 10 次失败应触发锁定")
	assert.Equal(t, 600, lockSeconds)

	remain, isLocked := l.CheckLocked(t.Context(), key)
	assert.True(t, isLocked)
	assert.Greater(t, remain, 0)

	// 锁定期间仍累计也不解除
	_, _ = l.RecordFailure(t.Context(), key)
	_, isLocked = l.CheckLocked(t.Context(), key)
	assert.True(t, isLocked)

	// 成功后 Reset 解除（对已锁定键 Reset 清计数；锁键独立存于 Redis 模式，内存模式锁按 TTL 自然到期）
	l.Reset(t.Context(), key)
}

func TestFormatLockKey(t *testing.T) {
	assert.Equal(t, FormatLockKey("a", "b"), FormatLockKey("a", "b"))
	assert.NotEqual(t, FormatLockKey("ab", "c"), FormatLockKey("a", "bc"), "组合键需防拼接歧义")
}
