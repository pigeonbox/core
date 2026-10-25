package middleware

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDefaultRateLimitConfig 默认配置
func TestDefaultRateLimitConfig(t *testing.T) {
	cfg := DefaultRateLimitConfig()
	assert.Equal(t, 100, cfg.GlobalQPS)
	assert.Equal(t, 10, cfg.UploadQPS)
	assert.Equal(t, 50, cfg.DownloadQPS)
	assert.Equal(t, 5, cfg.LoginQPS)
	assert.Equal(t, 20, cfg.Burst)
	assert.True(t, cfg.Enabled)
}

// TestRateLimiter_Allow_IPDimension IP 维度限流
func TestRateLimiter_Allow_IPDimension(t *testing.T) {
	cfg := RateLimitConfig{
		GlobalQPS: 5,
		Burst:     5,
		Enabled:   true,
	}
	rl := NewRateLimiter(cfg)
	defer rl.Stop()

	// 同一个 IP 在 burst 范围内允许
	for i := 0; i < 5; i++ {
		assert.True(t, rl.allow(t.Context(), scopeGlobal, "1.2.3.4"), "burst 内第 %d 次", i+1)
	}
	// burst 用完后拒绝
	assert.False(t, rl.allow(t.Context(), scopeGlobal, "1.2.3.4"), "burst 满后拒绝")
	assert.False(t, rl.allow(t.Context(), scopeGlobal, "1.2.3.4"), "持续拒绝")
}

// TestRateLimiter_Allow_ScopeDimension 不同 scope 独立计数
func TestRateLimiter_Allow_ScopeDimension(t *testing.T) {
	cfg := RateLimitConfig{
		GlobalQPS:   100,
		UploadQPS:   2,
		DownloadQPS: 100,
		LoginQPS:    100,
		Burst:       2,
		Enabled:     true,
	}
	rl := NewRateLimiter(cfg)
	defer rl.Stop()
	ip := "1.2.3.4"

	// Upload scope 用完 burst
	assert.True(t, rl.allow(t.Context(), scopeUpload, ip))
	assert.True(t, rl.allow(t.Context(), scopeUpload, ip))
	assert.False(t, rl.allow(t.Context(), scopeUpload, ip), "upload burst 用完")

	// Download scope 独立计数
	assert.True(t, rl.allow(t.Context(), scopeDownload, ip), "download scope 独立")
	assert.True(t, rl.allow(t.Context(), scopeDownload, ip))

	// Global scope 也独立
	assert.True(t, rl.allow(t.Context(), scopeGlobal, ip), "global scope 独立")
}

// TestRateLimiter_Allow_DifferentIP 不同 IP 独立
func TestRateLimiter_Allow_DifferentIP(t *testing.T) {
	cfg := RateLimitConfig{
		GlobalQPS: 1,
		Burst:     1,
		Enabled:   true,
	}
	rl := NewRateLimiter(cfg)
	defer rl.Stop()

	// IP A 用完 burst
	assert.True(t, rl.allow(t.Context(), scopeGlobal, "ip-A"))
	assert.False(t, rl.allow(t.Context(), scopeGlobal, "ip-A"))

	// IP B 独立
	assert.True(t, rl.allow(t.Context(), scopeGlobal, "ip-B"))
	assert.False(t, rl.allow(t.Context(), scopeGlobal, "ip-B"))
}

// TestRateLimiter_Allow_DisabledEnabled=false 全部放行
func TestRateLimiter_Allow_Disabled(t *testing.T) {
	cfg := RateLimitConfig{
		GlobalQPS: 1,
		Burst:     1,
		Enabled:   false,
	}
	rl := NewRateLimiter(cfg)
	defer rl.Stop()

	// 任何 IP 任何 scope 都能过（无数限制）
	for i := 0; i < 1000; i++ {
		assert.True(t, rl.allow(t.Context(), scopeGlobal, "1.2.3.4"))
	}
}

// TestRateLimiter_BurstCapacity 突发容量验证
func TestRateLimiter_BurstCapacity(t *testing.T) {
	cfg := RateLimitConfig{
		GlobalQPS: 10, // 10 QPS
		Burst:     20, // 突发 20
		Enabled:   true,
	}
	rl := NewRateLimiter(cfg)
	defer rl.Stop()
	ip := "burst-test"

	// 一次性消耗 20 个 token
	for i := 0; i < 20; i++ {
		assert.True(t, rl.allow(t.Context(), scopeGlobal, ip), "burst 第 %d 个", i+1)
	}
	// 21 个应该失败
	assert.False(t, rl.allow(t.Context(), scopeGlobal, ip))

	// 等 1 个 token 补充（QPS=10 → 100ms 一个 token）
	time.Sleep(150 * time.Millisecond)
	// 现在应该又有 1-2 个 token 可用
	allowed := rl.allow(t.Context(), scopeGlobal, ip)
	assert.True(t, allowed, "等待 100ms 后 burst 应至少 1 个 token")
}

// TestRateLimiter_UpdateConfig 热更新配置
func TestRateLimiter_UpdateConfig(t *testing.T) {
	cfg := RateLimitConfig{
		GlobalQPS: 1,
		Burst:     1,
		Enabled:   true,
	}
	rl := NewRateLimiter(cfg)
	defer rl.Stop()
	ip := "hot-reload"

	// 用完 burst
	assert.True(t, rl.allow(t.Context(), scopeGlobal, ip))
	assert.False(t, rl.allow(t.Context(), scopeGlobal, ip))

	// 热更新：提高 QPS 和 burst
	rl.UpdateConfig(RateLimitConfig{
		GlobalQPS: 100,
		Burst:     50,
		Enabled:   true,
	})

	// 现在应该又有 burst
	for i := 0; i < 50; i++ {
		assert.True(t, rl.allow(t.Context(), scopeGlobal, ip), "更新后第 %d 个", i+1)
	}
}

// TestRateLimiter_LoginQPS 登录接口独立低 QPS
func TestRateLimiter_LoginQPS(t *testing.T) {
	cfg := RateLimitConfig{
		GlobalQPS: 100,
		LoginQPS:  2,
		Burst:     2,
		Enabled:   true,
	}
	rl := NewRateLimiter(cfg)
	defer rl.Stop()

	assert.True(t, rl.allow(t.Context(), scopeLogin, "attacker"))
	assert.True(t, rl.allow(t.Context(), scopeLogin, "attacker"))
	assert.False(t, rl.allow(t.Context(), scopeLogin, "attacker"), "登录限速 2 QPS 触发")
}

// TestRateLimiter_ZeroQPS 边界：QPS=0 → 兜底 1
func TestRateLimiter_ZeroQPS(t *testing.T) {
	cfg := RateLimitConfig{
		UploadQPS: 0, // 0 应该兜底为 1
		Burst:     0, // 0 应该兜底为 QPS
		Enabled:   true,
	}
	rl := NewRateLimiter(cfg)
	defer rl.Stop()

	assert.True(t, rl.allow(t.Context(), scopeUpload, "1.2.3.4"), "第一次应通过")
	assert.False(t, rl.allow(t.Context(), scopeUpload, "1.2.3.4"), "第二次应被拒（QPS=1 burst=1）")
}

// TestRateLimiter_Concurrent 不同 IP 并发
func TestRateLimiter_Concurrent(t *testing.T) {
	cfg := RateLimitConfig{
		GlobalQPS: 50,
		Burst:     50,
		Enabled:   true,
	}
	rl := NewRateLimiter(cfg)
	defer rl.Stop()

	var allowed int32
	var wg sync.WaitGroup
	const workers = 20
	const perWorker = 5
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			ip := "concurrent-ip-" + string(rune('A'+workerID%26))
			for i := 0; i < perWorker; i++ {
				if rl.allow(t.Context(), scopeGlobal, ip) {
					atomic.AddInt32(&allowed, 1)
				}
			}
		}(w)
	}
	wg.Wait()
	// 20 个不同 IP × 5 次 = 100 次调用，每个 IP 限制 50 QPS
	// 期望：所有调用都通过（每个 IP 都用自己 50 的 burst）
	assert.Equal(t, int32(workers*perWorker), atomic.LoadInt32(&allowed),
		"每个 IP 独立 burst 50，20 个不同 IP 各 5 次都应通过")
}

// TestRateLimiter_Stop GC loop 关闭
func TestRateLimiter_Stop(t *testing.T) {
	rl := NewRateLimiter(DefaultRateLimitConfig())
	// Stop 应该正常关闭 stopCh，gc loop 退出
	rl.Stop()
}

// TestClientIP_XForwardedFor X-Forwarded-For 的采信口径。
//
// 直连可信时，从右向左取第一个非可信跳（XFF 左端是客户端可伪造的部分，
// 伪造值必须不被采信，否则换个头就换一个干净的限流桶）；直连不可信时，
// 代理头一律忽略（直连方伪造 XFF 即可绕过 IP 维度限流）。
//
// ⚠️ 本用例此前是**空测**：没有调用任何生产代码，只在测试里手工切字符串断言
// 首段是 "1.2.3.4"——测的是测试自己的字符串切分，与生产解析口径无关，且
// 注释里写的「第一个 IP 是客户端」正是被收编掉的不安全口径（左端可伪造）。
// 现改为直接打生产解析面。
func TestClientIP_XForwardedFor(t *testing.T) {
	require.NoError(t, SetTrustedProxies([]string{"10.0.0.0/8"}))
	t.Cleanup(func() { _ = SetTrustedProxies(nil) })

	// 直连可信 → 最右的非可信跳 1.2.3.4 是真实客户端，最左端 9.9.9.9 是伪造值
	assert.Equal(t, "1.2.3.4", ResolveClientIP("10.0.0.2:443", "9.9.9.9, 1.2.3.4, 10.0.0.1"))
	// 直连不可信 → 代理头一律忽略，按直连地址
	assert.Equal(t, "203.0.113.9", ResolveClientIP("203.0.113.9:443", "9.9.9.9, 1.2.3.4, 10.0.0.1"))
}

// TestRateLimiter_GC_NoError GC 不会 panic
func TestRateLimiter_GC_NoError(t *testing.T) {
	rl := NewRateLimiter(DefaultRateLimitConfig())
	defer rl.Stop()

	// 加几个 IP
	rl.allow(t.Context(), scopeGlobal, "ip-1")
	rl.allow(t.Context(), scopeGlobal, "ip-2")
	require.Equal(t, 2, rl.limiters[scopeGlobal].Len())

	// 手动调用 gc：只清封禁表，桶托管不过手——新鲜桶不会被淘汰
	// （KeyedLimiter 按 lastSeen TTL 自管，淘汰线见 limiterIdleTTL）
	rl.gc()
	assert.Equal(t, 2, rl.limiters[scopeGlobal].Len(), "新加的 entry 不会被清理")
}
