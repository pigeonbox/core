package bootstrap

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/conf"
)

// TestApplyDeploymentConstraints_FederationDisabledInMultiReplica 多副本拓扑下联邦
// 必须被降级关闭（节点身份是进程级 Ed25519 密钥，多副本语义未定义）。
//
// 这是「部署模式派生的硬约束」的**启动时**那一半；运行期那一半在
// app/config 的 applySystemConfigOverlayLocked（见 app/config/p2p_multireplica_test.go）——
// 两半共用 conf.FederationAllowedInMode 单一真相源。
func TestApplyDeploymentConstraints_FederationDisabledInMultiReplica(t *testing.T) {
	newCfg := func(mode string) *conf.AppConfiguration {
		cfg := &conf.AppConfiguration{}
		cfg.Deployment.Mode = mode
		cfg.Database.Driver = "mysql"
		cfg.Redis.Host = "redis"
		cfg.Federation.Enabled = true
		return cfg
	}

	t.Run("public 副本降级关闭联邦", func(t *testing.T) {
		cfg := newCfg(conf.DeployModePublic)
		require.NoError(t, applyDeploymentConstraints(cfg))
		assert.False(t, cfg.Federation.Enabled, "多副本下联邦必须被降级关闭")
	})

	t.Run("admin 面同样降级关闭联邦", func(t *testing.T) {
		cfg := newCfg(conf.DeployModeAdmin)
		require.NoError(t, applyDeploymentConstraints(cfg))
		assert.False(t, cfg.Federation.Enabled, "admin 面属多副本拓扑 ⇒ 联邦同样必须降级")
	})

	t.Run("standalone 保留联邦（负对照：不得写过头）", func(t *testing.T) {
		cfg := newCfg(conf.DeployModeStandalone)
		cfg.Database.Driver = "sqlite" // standalone 允许 sqlite
		require.NoError(t, applyDeploymentConstraints(cfg))
		assert.True(t, cfg.Federation.Enabled, "单机模式下联邦必须仍然可用")
	})
}
