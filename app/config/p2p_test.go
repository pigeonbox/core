package config

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/conf"
)

// TestValidateSystemConfigP2PFederation p2p.federation 段校验:URL 仅 http/https、
// registry 支持逗号多主备、熵门槛范围。写库前最后一道防线(同 notify/oidc 先例)。
func TestValidateSystemConfigP2PFederation(t *testing.T) {
	ok := func() *SystemConfig {
		return &SystemConfig{P2P: &P2PSection{Federation: &conf.FederationConfig{
			Enabled:     true,
			RegistryURL: "http://p2p1:12346,https://p2p2:56788",
			PublicURL:   "https://share.example.com",
		}}}
	}
	if err := validateSystemConfig(ok()); err != nil {
		t.Fatalf("合法配置被拒: %v", err)
	}
	badScheme := ok()
	badScheme.P2P.Federation.RegistryURL = "ftp://p2p:12346"
	if err := validateSystemConfig(badScheme); err == nil {
		t.Fatal("ftp scheme 应被拒")
	}
	badPub := ok()
	badPub.P2P.Federation.PublicURL = "javascript:alert(1)"
	if err := validateSystemConfig(badPub); err == nil {
		t.Fatal("非法 public_url 应被拒")
	}
	badEntropy := ok()
	badEntropy.P2P.Federation.AnnounceMinEntropyBits = -1
	if err := validateSystemConfig(badEntropy); err == nil {
		t.Fatal("负熵门槛应被拒")
	}
	if err := validateSystemConfig(&SystemConfig{P2P: &P2PSection{}}); err != nil {
		t.Fatalf("P2P 段无 federation(nil)应合法: %v", err)
	}
}

// withGlobalConf 测试期注入全局 conf（overlay 的落点）；t.Cleanup 还原空壳。
func withGlobalConf(t *testing.T) *conf.AppConfiguration {
	t.Helper()
	conf.SetGlobalConfig(&conf.AppConfiguration{})
	t.Cleanup(func() { conf.SetGlobalConfig(&conf.AppConfiguration{}) })
	return conf.GetGlobalConfig()
}

// TestUpdateConfigP2PPartialPreserveAndOverlay 局部保存不清零 p2p 段;
// overlay 把 federation 写进全局 conf;未提交 p2p 的保存保留现状。
func TestUpdateConfigP2PPartialPreserveAndOverlay(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	g := withGlobalConf(t)

	// 1) 提交 p2p.federation → 全局 conf 即刻热生效
	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		P2P: &P2PSection{Federation: &conf.FederationConfig{Enabled: true, RegistryURL: "http://p2p:12346"}},
	}))
	require.True(t, g.Federation.Enabled)
	require.Equal(t, "http://p2p:12346", g.Federation.RegistryURL)

	// 2) 局部保存(只带 oidc)→ p2p 段保留,不回弹
	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{OIDC: &conf.OIDCConfig{}}))
	got, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, got.P2P)
	require.True(t, got.P2P.Federation.Enabled)
	require.True(t, g.Federation.Enabled)

	// 3) 显式改回关闭 → overlay 跟随
	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		P2P: &P2PSection{Federation: &conf.FederationConfig{Enabled: false}},
	}))
	require.False(t, g.Federation.Enabled)
}

// TestUpdateConfigP2PFederationHook 热切换钩子只在 p2p.federation 非 nil 时触发。
func TestUpdateConfigP2PFederationHook(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	withGlobalConf(t)
	var got *conf.FederationConfig
	svc.SetReconfigureHooks(&ReconfigureHooks{
		OnP2PFederationChanged: func(f *conf.FederationConfig) { got = f },
	})
	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{OIDC: &conf.OIDCConfig{}}))
	require.Nil(t, got, "未提交 p2p 段不应触发")
	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		P2P: &P2PSection{Federation: &conf.FederationConfig{Enabled: true}},
	}))
	require.NotNil(t, got)
	require.True(t, got.Enabled)
}
