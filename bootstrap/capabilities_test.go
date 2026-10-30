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
	"encoding/json"
	"testing"

	"github.com/pigeonbox/core/conf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	assert.False(t, full.Federation, "联邦不在公开快照里（顶层 federationEnabled 才是它的家）")
	assert.True(t, full.RedisMode == "redis", "完整快照才带 Redis 模式")

	// 公开面下发的 JSON **恰好三个键** —— 管理面事实（Redis 模式/存储后端）
	// 连空串形态都不许泄出去（`redis_mode:""` 既不是事实也不是可辨识的「未知」）。
	raw, err := json.Marshal(pub)
	require.NoError(t, err)
	var keys map[string]any
	require.NoError(t, json.Unmarshal(raw, &keys))
	assert.Len(t, keys, 3, "公开能力段只允许三个键：%s", string(raw))
	for _, k := range []string{"deployment_mode", "public_url_set", "setup_token_required"} {
		assert.Contains(t, keys, k)
	}
	assert.NotContains(t, keys, "redis_mode")
	assert.NotContains(t, keys, "storage_backend")
	assert.NotContains(t, keys, "federation")
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
