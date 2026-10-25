package bootstrap

import (
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/conf"
)

// 波次3：security.api_token.* 环境变量绑定（显式映射表，非 AutomaticEnv）。
func TestBindEnvironment_APIToken(t *testing.T) {
	t.Setenv("PB_API_TOKEN_PER_KEY_QPS", "1")
	t.Setenv("PB_API_TOKEN_PER_KEY_BURST", "3")

	v := viper.New()
	v.SetDefault("security.api_token.enabled", true)
	v.SetDefault("security.api_token.per_key_qps", 20)
	v.SetDefault("security.api_token.per_key_burst", 40)
	bindEnvironment(v)

	var cfg conf.AppConfiguration
	require.NoError(t, v.Unmarshal(&cfg))
	assert.Equal(t, 1, cfg.Security.APIToken.PerKeyQPS, "env 必须覆盖 SetDefault")
	assert.Equal(t, 3, cfg.Security.APIToken.PerKeyBurst)
	assert.True(t, cfg.Security.APIToken.Enabled)
}

// TestBindEnvironment_EnableHSTS_TriState HSTS 三态绑定（2026-10-08 加固）：
// env 字符串必须能正确解到 *bool（viper WeaklyTypedInput），未设置时保持 nil
// 交由 bootstrap 按 IsProduction 裁决。
func TestBindEnvironment_EnableHSTS_TriState(t *testing.T) {
	t.Run("env_true", func(t *testing.T) {
		t.Setenv("PB_ENABLE_HSTS", "true")
		v := viper.New()
		bindEnvironment(v)
		var cfg conf.AppConfiguration
		require.NoError(t, v.Unmarshal(&cfg))
		require.NotNil(t, cfg.Security.CORS.EnableHSTS)
		assert.True(t, *cfg.Security.CORS.EnableHSTS)
	})
	t.Run("env_false", func(t *testing.T) {
		t.Setenv("PB_ENABLE_HSTS", "false")
		v := viper.New()
		bindEnvironment(v)
		var cfg conf.AppConfiguration
		require.NoError(t, v.Unmarshal(&cfg))
		require.NotNil(t, cfg.Security.CORS.EnableHSTS)
		assert.False(t, *cfg.Security.CORS.EnableHSTS, "显式 false 必须能关掉生产默认")
	})
	t.Run("unset_nil", func(t *testing.T) {
		v := viper.New()
		bindEnvironment(v)
		var cfg conf.AppConfiguration
		require.NoError(t, v.Unmarshal(&cfg))
		assert.Nil(t, cfg.Security.CORS.EnableHSTS, "未配置必须保持 nil（三态语义的前提）")
	})
}

// TestBindEnvironment_CORSAllowOriginsCSV CORS 白名单的 env 绑定（列表值按 CSV 拆）。
//
// 背景（2026-10-25 删除旧版本兼容）：bootstrap/middleware.go 曾**再读一次**
// PB_CORS_ALLOW_ORIGINS 手工拼白名单（注释写「兼容 slice 字段在 env 下的传递」），
// 而 config 层（bindEnvironment + listValuedKeys）**早已**把同一个 env 按 CSV 拆进
// security.cors.allow_origins ⇒ 那段 env 读取是**完全冗余**，已删。
//
// ⇒ 删掉后，CORS 白名单的唯一来源是配置层 ⇒ 必须有回归网钉住这条链路，
// 否则将来「env 传多值」会静默失效（CORS 退化为反射 Origin ⇒ 安全语义放宽）。
func TestBindEnvironment_CORSAllowOriginsCSV(t *testing.T) {
	t.Setenv("PB_CORS_ALLOW_ORIGINS", "https://a.example.com, https://b.example.com ,")

	v := viper.New()
	v.SetDefault("security.cors.allow_origins", []string{})
	bindEnvironment(v)

	var cfg conf.AppConfiguration
	require.NoError(t, v.Unmarshal(&cfg))
	assert.Equal(t, []string{"https://a.example.com", "https://b.example.com"}, cfg.Security.CORS.AllowOrigins,
		"PB_CORS_ALLOW_ORIGINS 必须按 CSV 拆进配置（middleware 不再自己读 env）")
}
