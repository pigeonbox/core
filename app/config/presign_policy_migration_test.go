package config

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/repo/db/model"
)

// TestDetectLegacyPresignAnonymous 回归：旧开关只从**原文**检出（解码后的结构体
// 里已经没有该字段）。
func TestDetectLegacyPresignAnonymous(t *testing.T) {
	raw := `{"download":{"presign_anonymous_enabled":false,"presign_policy":"everyone"}}`
	v, ok := detectLegacyPresignAnonymous(raw)
	require.True(t, ok)
	assert.False(t, v.Enabled)

	raw = `{"download":{"presign_anonymous_enabled":true}}`
	v, ok = detectLegacyPresignAnonymous(raw)
	require.True(t, ok)
	assert.True(t, v.Enabled)

	// 无该键
	_, ok = detectLegacyPresignAnonymous(`{"download":{"presign_policy":"everyone"}}`)
	assert.False(t, ok)

	// 非法 JSON
	_, ok = detectLegacyPresignAnonymous(`{`)
	assert.False(t, ok)
}

// TestLoadPersistedConfig_AppliesLegacyPresignPolicy 回归**接线点**：持久化原文里
// 带旧开关时，读配置路径必须把语义并入 presign_policy（否则「曾关闭匿名直传」
// 的站点升级后静默变开放）。
func TestLoadPersistedConfig_AppliesLegacyPresignPolicy(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()

	// 直接写一条带旧开关的持久化记录（模拟存量库）
	rec := &model.SystemConfigRecord{Data: `{"download":{"presign_anonymous_enabled":false}}`}
	require.NoError(t, svc.configRepo.Save(ctx, rec))

	got, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, got.Download)
	assert.Equal(t, conf.PresignPolicyAuthenticated, got.Download.PresignPolicy,
		"旧开关 false 必须并入策略（不得静默变成 everyone）")
}

// TestApplyLegacyPresignPolicy 回归：旧开关语义并入新策略——
//   - 新策略已设置 ⇒ 以新策略为准（管理员改过）
//   - 新策略为空 ⇒ 由旧开关推导（false⇒authenticated，true⇒everyone）
func TestApplyLegacyPresignPolicy(t *testing.T) {
	// 旧开关 false + 新策略未设置 ⇒ 仅登录用户可直传（不得静默变开放）
	cfg := &SystemConfig{Download: &conf.DownloadConfig{}}
	applyLegacyPresignPolicy(cfg, `{"download":{"presign_anonymous_enabled":false}}`)
	assert.Equal(t, conf.PresignPolicyAuthenticated, cfg.Download.PresignPolicy)

	// 旧开关 true + 新策略未设置 ⇒ 所有人
	cfg = &SystemConfig{Download: &conf.DownloadConfig{}}
	applyLegacyPresignPolicy(cfg, `{"download":{"presign_anonymous_enabled":true}}`)
	assert.Equal(t, conf.PresignPolicyEveryone, cfg.Download.PresignPolicy)

	// 新策略已设置 ⇒ 旧开关忽略
	cfg = &SystemConfig{Download: &conf.DownloadConfig{PresignPolicy: conf.PresignPolicyDisabled}}
	applyLegacyPresignPolicy(cfg, `{"download":{"presign_anonymous_enabled":false}}`)
	assert.Equal(t, conf.PresignPolicyDisabled, cfg.Download.PresignPolicy)

	// 无 download 段 ⇒ 不动
	cfg = &SystemConfig{}
	applyLegacyPresignPolicy(cfg, `{"base":{}}`)
	assert.Nil(t, cfg.Download)

	// 旧键不会残留在结构体里（JSON 序列化即清掉）
	cfg = &SystemConfig{Download: &conf.DownloadConfig{}}
	applyLegacyPresignPolicy(cfg, `{"download":{"presign_anonymous_enabled":false}}`)
	data, err := json.Marshal(cfg)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "presign_anonymous_enabled")
}
