package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/internal/sectest"
)

// core#81 集成判据：InitConfig 的解码器直接产出混淆持有器——
// client_secret 明文自解码起不在 conf 结构体驻留（hook 组合经由 viper）。

const bootstrapSecretMarker = "init-config-secret-2f6e"

func writeTestConfigYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

// TestInitConfig_OIDCSecretObfuscated 解码后：持有器产出、混淆存储不含明文、
// 取回原文；普通字段（issuer）解码行为不变。
func TestInitConfig_OIDCSecretObfuscated(t *testing.T) {
	p := writeTestConfigYAML(t, `
user:
  jwt_secret: test-only-jwt-secret-not-a-default-value-0123456789
security:
  oidc:
    enabled: true
    issuer: https://idp.example.com
    client_id: client-1
    client_secret: `+bootstrapSecretMarker+`
`)

	cfg, err := InitConfig(p)
	require.NoError(t, err)
	oidcCfg := cfg.Security.OIDC
	require.NotNil(t, oidcCfg.ClientSecret, "解码钩子必须产出持有器")

	// 对照：普通 string 字段原样解码（钩子只对 SecureString 目标生效）。
	assert.Equal(t, "https://idp.example.com", oidcCfg.Issuer)
	assert.Equal(t, "client-1", oidcCfg.ClientID)

	sectest.AssertNotStoredInPlaintext(t, oidcCfg.ClientSecret, bootstrapSecretMarker)
}

// TestInitConfig_HookCompositionPreserved viper.DecodeHook 选项会整体覆盖默认
// 钩子 ⇒ 组合必须带上 duration/逗号切片钩子。逗号字符串 → []string（如
// trusted_proxies）是 viper 默认钩子的既有行为，回归锁。
func TestInitConfig_HookCompositionPreserved(t *testing.T) {
	p := writeTestConfigYAML(t, `
user:
  jwt_secret: test-only-jwt-secret-not-a-default-value-0123456789
security:
  trusted_proxies: "10.0.0.0/8,172.16.0.0/12"
  oidc:
    enabled: true
    client_secret: `+bootstrapSecretMarker+`
`)

	cfg, err := InitConfig(p)
	require.NoError(t, err)
	assert.Equal(t, []string{"10.0.0.0/8", "172.16.0.0/12"}, cfg.Security.TrustedProxies,
		"组合层必须保留 viper 默认的逗号切片钩子")
	require.NotNil(t, cfg.Security.OIDC.ClientSecret)
	assert.Equal(t, bootstrapSecretMarker, cfg.Security.OIDC.ClientSecret.Get())
}

// TestInitConfig_OIDCSecretAbsent 键缺席 ⇒ 持有器保持 nil（不得凭空建）；
// 读取点对 nil 的容错由 app/oidc.NewService 归一兜底。
func TestInitConfig_OIDCSecretAbsent(t *testing.T) {
	p := writeTestConfigYAML(t, `
user:
  jwt_secret: test-only-jwt-secret-not-a-default-value-0123456789
security:
  oidc:
    enabled: true
`)
	cfg, err := InitConfig(p)
	require.NoError(t, err)
	assert.Nil(t, cfg.Security.OIDC.ClientSecret)

	// 空串 ⇒ 空持有器（未配置语义）。
	p2 := writeTestConfigYAML(t, `
user:
  jwt_secret: test-only-jwt-secret-not-a-default-value-0123456789
security:
  oidc:
    client_secret: ""
`)
	cfg2, err := InitConfig(p2)
	require.NoError(t, err)
	require.NotNil(t, cfg2.Security.OIDC.ClientSecret)
	assert.Empty(t, cfg2.Security.OIDC.ClientSecret.Get())
}

// TestReapplyEnvOverrides_OIDCSecretLeaf env 恒覆盖对持有器叶子生效（core#81
// 叶子扩展点）：env 值整只替换进 conf，明文混淆存储。
func TestReapplyEnvOverrides_OIDCSecretLeaf(t *testing.T) {
	prev := conf.Snapshot()
	conf.SetGlobalConfig(&conf.AppConfiguration{})
	t.Cleanup(func() { conf.SetGlobalConfig(prev) })

	// yaml/DB 值先行（作为被 env 覆盖的一方）
	seeded := conf.NewSecretString("db-overlay-value")
	conf.UpdateGlobalConfig(func(g *conf.AppConfiguration) {
		g.Security.OIDC.ClientSecret = seeded
	})

	t.Setenv("PB_OIDC_CLIENT_SECRET", bootstrapSecretMarker)
	ReapplyEnvOverrides()

	got := conf.Snapshot().Security.OIDC.ClientSecret
	require.NotNil(t, got)
	sectest.AssertNotStoredInPlaintext(t, got, bootstrapSecretMarker)
	assert.NotSame(t, seeded, got, "整只替换：env 覆盖后 conf 持新持有器")
	assert.Equal(t, "db-overlay-value", seeded.Get(), "旧持有器内容不受影响（Clear 由 overlay 守卫负责）")

	// nil 持有器同样可写
	conf.UpdateGlobalConfig(func(g *conf.AppConfiguration) {
		g.Security.OIDC.ClientSecret = nil
	})
	ReapplyEnvOverrides()
	got2 := conf.Snapshot().Security.OIDC.ClientSecret
	require.NotNil(t, got2)
	assert.Equal(t, bootstrapSecretMarker, got2.Get())
}
