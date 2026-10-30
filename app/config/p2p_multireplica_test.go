package config

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/conf"
)

// 多副本拓扑下的联邦开关：**部署模式派生的硬约束**不得被管理台的提交意图复活。
//
// # 缺陷形态
//
// bootstrap/deployment.go 的 applyDeploymentConstraints 在**启动时**把多副本的
// federation.Enabled 强制关掉（节点身份是进程级 Ed25519 密钥，多副本语义未定义）。
//
// 但 applySystemConfigOverlayLocked 是**整段赋值**（next.Federation = *sc.P2P.Federation），
// DB 里存着的 enabled=true 会被原样盖进来 ⇒ 启动时的降级被**复活**。
//
// 后果链：全局 conf 的 Federation.Enabled 变回 true ⇒ /api/config 报
// federationEnabled=true ⇒ 前端在多副本拓扑下展示「本柜与其它站点联网：凭取件码取件时
// 若本柜没有，会自动到联网站点查找」⇒ **承诺了一个架构上不可用的跨站取件能力**。
//
// # 触发时机
//
// 不止启动那一刻：
//
//   - 管理台**任意一次**保存（哪怕只带了 oidc 段）都会走 overlay；
//   - **多副本配置广播**（applyPropagatedConfig → RestoreAdminSettings → overlay）
//     在每次配置变更后都会走。
//
// ⇒ 只要 DB 里存着 enabled=true，多副本实例上它就会被反复复活。

// setGlobalDeploymentMode 设置全局 conf 的部署模式（测试期）。
func setGlobalDeploymentMode(t *testing.T, mode string) {
	t.Helper()
	conf.UpdateGlobalConfig(func(next *conf.AppConfiguration) {
		next.Deployment.Mode = mode
	})
}

// TestOverlayMultiReplicaKeepsFederationDisabled 多副本下 overlay 不得复活联邦开关。
func TestOverlayMultiReplicaKeepsFederationDisabled(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	withGlobalConf(t)
	setGlobalDeploymentMode(t, conf.DeployModePublic)

	var hooked *conf.FederationConfig
	svc.SetReconfigureHooks(&ReconfigureHooks{
		OnP2PFederationChanged: func(f *conf.FederationConfig) error { hooked = f; return nil },
	})

	// 管理台提交「开启联邦」——DB 里存的是**提交意图**
	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		P2P: &P2PSection{Federation: &conf.FederationConfig{Enabled: true, RegistryURL: "http://p2p:12346"}},
	}))

	require.False(t, conf.Snapshot().Federation.Enabled,
		"多副本下 overlay 不得复活被部署约束降级的联邦开关")

	// 运行期热切换钩子拿到的也必须是**生效值**（否则联邦运行期真被启动，
	// 与「全局 conf 说关」分叉：节点照样注册进注册中心）
	require.NotNil(t, hooked, "提交了 p2p 段 ⇒ 钩子必须触发")
	require.False(t, hooked.Enabled, "多副本下热切换钩子不得启动联邦")
}

// TestOverlayAdminReplicaKeepsFederationDisabled admin 面同样是多副本拓扑，
// 约束对 public / admin 一视同仁。
func TestOverlayAdminReplicaKeepsFederationDisabled(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	withGlobalConf(t)
	setGlobalDeploymentMode(t, conf.DeployModeAdmin)

	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		P2P: &P2PSection{Federation: &conf.FederationConfig{Enabled: true}},
	}))
	require.False(t, conf.Snapshot().Federation.Enabled,
		"admin 面属多副本拓扑 ⇒ 联邦同样必须恒关")
}

// TestOverlayStandaloneStillEnablesFederation 负对照：standalone 下管理台**仍要能**
// 开启联邦。防止把约束写过头（无差别关联邦 = 把功能也废了）。
func TestOverlayStandaloneStillEnablesFederation(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	withGlobalConf(t)
	setGlobalDeploymentMode(t, conf.DeployModeStandalone)

	var hooked *conf.FederationConfig
	svc.SetReconfigureHooks(&ReconfigureHooks{
		OnP2PFederationChanged: func(f *conf.FederationConfig) error { hooked = f; return nil },
	})

	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		P2P: &P2PSection{Federation: &conf.FederationConfig{Enabled: true, RegistryURL: "http://p2p:12346"}},
	}))
	require.True(t, conf.Snapshot().Federation.Enabled,
		"standalone 下管理台开启联邦必须仍然生效（约束不得写过头）")
	require.NotNil(t, hooked)
	require.True(t, hooked.Enabled, "standalone 下钩子必须拿到 enabled=true")
}

// TestOverlayMultiReplicaKeepsOtherFederationFields 多副本下被强制的**只有**
// enabled 开关；registry_url 等其余字段照常 overlay（不得整段丢弃，
// 否则管理台存的对端地址会凭空消失）。
func TestOverlayMultiReplicaKeepsOtherFederationFields(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	withGlobalConf(t)
	setGlobalDeploymentMode(t, conf.DeployModePublic)

	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		P2P: &P2PSection{Federation: &conf.FederationConfig{
			Enabled:     true,
			RegistryURL: "http://p2p:12346",
			PublicURL:   "https://share.example.com",
		}},
	}))
	got := conf.Snapshot().Federation
	require.False(t, got.Enabled)
	require.Equal(t, "http://p2p:12346", got.RegistryURL)
	require.Equal(t, "https://share.example.com", got.PublicURL)
}

// TestFederationAllowedInMode 单一真相源本身：只有 standalone 允许联邦。
func TestFederationAllowedInMode(t *testing.T) {
	require.True(t, conf.FederationAllowedInMode(conf.DeployModeStandalone))
	require.True(t, conf.FederationAllowedInMode(""), "空模式按 standalone 归一（与 NormalizedDeploymentMode 同一约定）")
	require.False(t, conf.FederationAllowedInMode(conf.DeployModePublic))
	require.False(t, conf.FederationAllowedInMode(conf.DeployModeAdmin))
	require.False(t, conf.FederationAllowedInMode("bogus"), "未知模式不得当作允许")
}
