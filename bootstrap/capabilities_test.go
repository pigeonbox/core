package bootstrap

// 能力自描述（2026-10-30）的断言。
//
// 守卫点（三个）：
//  1. **公开面只放匿名可见也无害的三项**——存储后端 / Redis 模式等管理面事实
//     不得随 /api/config 泄出；
//  2. **不重复下发**：顶层已有同义字段的（federationEnabled 等）不在能力快照里
//     再来一份——同一事实两个字段必然漂移（presignEnabled 的教训）；
//  3. 部署模式 / 站点地址 / 首启令牌三个开关必须**如实**反映生效值。

import (
	"context"
	"testing"

	"github.com/pigeonbox/core/conf"
	"github.com/stretchr/testify/assert"
)

func TestPublicCapabilitySnapshot_OnlyPublicSafeFields(t *testing.T) {
	resetSetupAutoToken(t)
	t.Setenv(setupTokenEnv, "")
	t.Setenv(setupTokenAutoEnv, "")
	withTestConfig(t, &conf.AppConfiguration{
		App:        conf.AppConfig{DataPath: t.TempDir()},
		Deployment: conf.DeploymentConfig{Mode: conf.DeployModePublic},
		Redis:      conf.RedisConfig{Host: "redis.internal"},
		Storage:    conf.StorageConfig{Type: "s3"},
	})

	full := capabilitySnapshot(context.Background())
	pub := publicCapabilitySnapshot(context.Background())

	assert.Equal(t, conf.DeployModePublic, pub.DeploymentMode, "部署模式必须如实下发")
	assert.Empty(t, pub.RedisMode, "公开面不得下发 Redis 模式（管理面事实）")
	assert.Empty(t, pub.StorageBackend, "公开面不得下发存储后端（管理面事实）")
	assert.False(t, pub.Federation, "联邦不在公开快照里（顶层 federationEnabled 才是它的家）")
	assert.True(t, full.RedisMode == "redis", "完整快照才带 Redis 模式")
}

func TestCapabilitySnapshot_DeploymentMode(t *testing.T) {
	resetSetupAutoToken(t)
	withTestConfig(t, &conf.AppConfiguration{
		Deployment: conf.DeploymentConfig{Mode: conf.DeployModeAdmin},
	})
	snap := capabilitySnapshot(context.Background())
	assert.Equal(t, conf.DeployModeAdmin, snap.DeploymentMode)
}

func TestCapabilitySnapshot_PublicURLSet(t *testing.T) {
	resetSetupAutoToken(t)
	t.Setenv(setupTokenEnv, "")
	t.Setenv(setupTokenAutoEnv, "")

	withTestConfig(t, &conf.AppConfiguration{})
	assert.False(t, capabilitySnapshot(context.Background()).PublicURLSet,
		"未配置站点公网地址 ⇒ public_url_set=false（分享链接会按请求 origin 拼 ⇒ 内网 IP 外发）")

	withTestConfig(t, &conf.AppConfiguration{
		Server: conf.ServerConfig{BaseURL: "https://box.example.com"},
	})
	assert.True(t, capabilitySnapshot(context.Background()).PublicURLSet)
}

func TestCapabilitySnapshot_SetupTokenRequired(t *testing.T) {
	resetSetupAutoToken(t)
	t.Setenv(setupTokenEnv, "some-token")
	t.Setenv(setupTokenAutoEnv, "")
	withTestConfig(t, &conf.AppConfiguration{})
	assert.True(t, capabilitySnapshot(context.Background()).SetupTokenRequired,
		"显式令牌生效 ⇒ setup_token_required=true")

	t.Setenv(setupTokenEnv, "")
	resetSetupAutoToken(t)
	assert.False(t, capabilitySnapshot(context.Background()).SetupTokenRequired,
		"无任何令牌 ⇒ setup_token_required=false")
}

func TestCapabilitySnapshot_StorageDegradedDetected(t *testing.T) {
	resetSetupAutoToken(t)
	withTestConfig(t, &conf.AppConfiguration{
		Storage: conf.StorageConfig{Type: "local"},
	})
	snap := capabilitySnapshot(context.Background())
	assert.Equal(t, "local", snap.StorageBackend)
	assert.False(t, snap.StorageDegraded, "配置 local + 生效 local ⇒ 未降级")
}
