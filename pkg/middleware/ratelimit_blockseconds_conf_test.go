package middleware

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/conf"
)

// withRateLimitSettings 注入 rate_limit 配置段并在用例结束时还原（core#152 判据用）。
func withRateLimitSettings(t *testing.T, s conf.RateLimitSettings) {
	t.Helper()
	old := conf.GetGlobalConfig()
	t.Cleanup(func() { conf.SetGlobalConfig(old) })
	conf.SetGlobalConfig(&conf.AppConfiguration{RateLimit: s})
}

// TestRateLimitConfigFromConf_ExplicitZeroBlockSeconds core#152 正判据：
// 显式配置 block_seconds=0 必须生效（契约 0=不封禁），不得被 >0 守卫静默吞掉回落 60。
// 两层断言：① 合并读数 ② 行为级——打满限流后确实不产生封禁（"真不封禁"）。
// 负对照：合并守卫回退成 `>0` ⇒ 本用例两层断言同时转红。
func TestRateLimitConfigFromConf_ExplicitZeroBlockSeconds(t *testing.T) {
	zero := 0
	withRateLimitSettings(t, conf.RateLimitSettings{BlockSeconds: &zero})

	cfg := RateLimitConfigFromConf()
	require.Equal(t, 0, cfg.BlockSeconds, "显式 0 必须生效：契约 block_seconds 0=不封禁，不得回落默认 60")

	// 行为级：配置 0 ⇒ 命中限流也不封禁（限流本身仍生效，封禁不发生）
	cfg.GlobalQPS, cfg.Burst, cfg.Enabled = 1, 1, true
	rl := NewRateLimiter(cfg)
	defer rl.Stop()
	ctx := t.Context()
	ip := "10.9.9.1"
	require.True(t, rl.allow(ctx, scopeGlobal, ip), "首个请求放行")
	require.False(t, rl.allow(ctx, scopeGlobal, ip), "burst 打满后拒绝（限流本身仍生效）")
	assert.False(t, rl.isBlocked(ctx, scopeGlobal, ip), "block_seconds=0：限流命中后不得封禁")
}

// TestRateLimitConfigFromConf_UnsetAndNegativeKeepDefault core#152 边界：
// 未配置（nil）与显式负数都保持既有语义——归默认 60（旧 >0 守卫的防负数行为不回退）。
func TestRateLimitConfigFromConf_UnsetAndNegativeKeepDefault(t *testing.T) {
	t.Run("未配置归默认", func(t *testing.T) {
		withRateLimitSettings(t, conf.RateLimitSettings{})
		assert.Equal(t, 60, RateLimitConfigFromConf().BlockSeconds, "未配置 ⇒ 默认 60")
	})
	t.Run("负数归默认", func(t *testing.T) {
		neg := -5
		withRateLimitSettings(t, conf.RateLimitSettings{BlockSeconds: &neg})
		assert.Equal(t, 60, RateLimitConfigFromConf().BlockSeconds, "负数非法 ⇒ 保持既有语义归默认 60")
	})
}
