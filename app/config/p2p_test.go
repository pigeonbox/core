package config

import (
	"context"
	"errors"
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
//
// ⚠️ 自 2026-10-31 起全局配置改为 **copy-on-write**（原子换指针），
// 因此**不能再长期持有一个快照指针**去断言"生效值"——热应用会换指针，
// 旧指针会停在旧值。断言一律读 conf.Snapshot()（当前快照）。
func withGlobalConf(t *testing.T) {
	t.Helper()
	conf.SetGlobalConfig(&conf.AppConfiguration{})
	t.Cleanup(func() { conf.SetGlobalConfig(&conf.AppConfiguration{}) })
}

// setGlobalFederation 在**当前快照的副本**上改 federation（copy-on-write 写侧）。
func setGlobalFederation(f conf.FederationConfig) {
	conf.UpdateGlobalConfig(func(next *conf.AppConfiguration) {
		next.Federation = f
	})
}

// TestUpdateConfigP2PPartialPreserveAndOverlay 局部保存不清零 p2p 段;
// overlay 把 federation 写进全局 conf;未提交 p2p 的保存保留现状。
func TestUpdateConfigP2PPartialPreserveAndOverlay(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	withGlobalConf(t)

	// 1) 提交 p2p.federation → 全局 conf 即刻热生效
	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		P2P: &P2PSection{Federation: &conf.FederationConfig{Enabled: true, RegistryURL: "http://p2p:12346"}},
	}))
	require.True(t, conf.Snapshot().Federation.Enabled)
	require.Equal(t, "http://p2p:12346", conf.Snapshot().Federation.RegistryURL)

	// 2) 局部保存(只带 oidc)→ p2p 段保留,不回弹
	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{OIDC: &conf.OIDCConfig{}}))
	got, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, got.P2P)
	require.True(t, got.P2P.Federation.Enabled)
	require.True(t, conf.Snapshot().Federation.Enabled)

	// 3) 显式改回关闭 → overlay 跟随
	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		P2P: &P2PSection{Federation: &conf.FederationConfig{Enabled: false}},
	}))
	require.False(t, conf.Snapshot().Federation.Enabled)
}

// TestOverlayBackfillsP2PFromGlobalConf DB 无 p2p 段时，overlay 从全局 conf
// 回填生效值（env 种子镜像；管理后台 GET 不显示成"关"）。
func TestOverlayBackfillsP2PFromGlobalConf(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	withGlobalConf(t)
	setGlobalFederation(conf.FederationConfig{Enabled: true, RegistryURL: "http://seed:56788"})

	// 任意局部保存（只带 oidc）触发 overlay → p2p 段被回填为 env 种子镜像
	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{OIDC: &conf.OIDCConfig{}}))
	got, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, got.P2P)
	require.NotNil(t, got.P2P.Federation)
	require.True(t, got.P2P.Federation.Enabled)
	require.Equal(t, "http://seed:56788", got.P2P.Federation.RegistryURL)
}

// TestUpdateConfigP2PFederationHook 热切换钩子只在 p2p.federation 非 nil 时触发。
func TestUpdateConfigP2PFederationHook(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	withGlobalConf(t)
	var got *conf.FederationConfig
	svc.SetReconfigureHooks(&ReconfigureHooks{
		OnP2PFederationChanged: func(f *conf.FederationConfig) error { got = f; return nil },
	})
	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{OIDC: &conf.OIDCConfig{}}))
	require.Nil(t, got, "未提交 p2p 段不应触发")
	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		P2P: &P2PSection{Federation: &conf.FederationConfig{Enabled: true}},
	}))
	require.NotNil(t, got)
	require.True(t, got.Enabled)
}

// TestUpdateConfigP2PFederationHookError 热切换失败必须透传给管理员——
// 配置已写库但运行态维持原状的分叉禁止静默（评审 P1-1）。
func TestUpdateConfigP2PFederationHookError(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	withGlobalConf(t)
	svc.SetReconfigureHooks(&ReconfigureHooks{
		OnP2PFederationChanged: func(f *conf.FederationConfig) error {
			return errors.New("boom：federation 装配失败")
		},
	})
	err := svc.UpdateConfig(ctx, &SystemConfig{
		P2P: &P2PSection{Federation: &conf.FederationConfig{Enabled: true, PublicURL: "http://s.example.com"}},
	})
	require.Error(t, err, "切换失败必须让保存请求失败（响亮）")
	require.Contains(t, err.Error(), "boom")
}

// TestUpdateConfigP2PSubsectionPreserve {"p2p":{}}（federation 子段 nil）不得冲掉
// 联邦配置——子段互冲是定时炸弹（同顶层指针段语义下沉一层）。
func TestUpdateConfigP2PSubsectionPreserve(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	withGlobalConf(t)
	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		P2P: &P2PSection{Federation: &conf.FederationConfig{Enabled: true, RegistryURL: "http://p2p:12346"}},
	}))
	// 只带空 p2p 段（无 federation 子段）
	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{P2P: &P2PSection{}}))
	got, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, got.P2P)
	require.NotNil(t, got.P2P.Federation, "空 p2p 段不应冲掉 federation 子段")
	require.True(t, got.P2P.Federation.Enabled)
	require.Equal(t, "http://p2p:12346", got.P2P.Federation.RegistryURL)
}

// TestDetectStaleP2PDirect 陈旧 p2p.direct 段检出（Wave B' 迁移语义）：
// 段存在即检出并带出具体键名与 registry_url 值；无 direct 段 = 未检出。
//
// 为什么要检出：direct 段已从 SystemConfig 删除，json 解码**静默忽略**未知键
// —— 只配了 p2p.direct.registry_url 的存量用户会静默失去在线名录（注册中心
// 真相源只剩 p2p.federation.registry_url）。
func TestDetectStaleP2PDirect(t *testing.T) {
	// 1) 无 direct 段 → 不检出
	_, ok := detectStaleP2PDirect(`{"p2p":{"federation":{"enabled":true}}}`)
	require.False(t, ok, "无 p2p.direct 段不应检出")

	// 2) 无 p2p 段 → 不检出
	_, ok = detectStaleP2PDirect(`{"base":{"name":"x"}}`)
	require.False(t, ok, "无 p2p 段不应检出")

	// 3) direct 段存在（尤其 registry_url 非空）→ 检出，键名与值齐
	stale, ok := detectStaleP2PDirect(
		`{"p2p":{"direct":{"enabled":true,"registry_url":"http://p2p:56788","send_roots":["/data"]}}}`)
	require.True(t, ok, "存量 direct 段必须被检出")
	require.Equal(t, "http://p2p:56788", stale.RegistryURL)
	require.Equal(t, []string{"enabled", "registry_url", "send_roots"}, stale.Keys)
	require.Equal(t,
		[]string{"p2p.direct.enabled", "p2p.direct.registry_url", "p2p.direct.send_roots"},
		stale.qualifiedKeys())

	// 4) 非法 JSON → 不检出（不炸）
	_, ok = detectStaleP2PDirect(`not-json`)
	require.False(t, ok)

	// 5) 段存在但 registry_url 为空串 → 仍检出（键名在，值为空）
	stale, ok = detectStaleP2PDirect(`{"p2p":{"direct":{"registry_url":""}}}`)
	require.True(t, ok)
	require.Equal(t, "", stale.RegistryURL)
}
