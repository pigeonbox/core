package config

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/internal/sectest"
)

// 本文件锁住 core#81 的管理台读写两条链：
// 读响应不吐明文（负对照：原始序列化必然扫得到）/ 写侧掩码哨兵回填与
// 值等复用 / 热更旋转旧持有器零化。

const sanitizeMarker = "admin-leak-marker-4e9a"

func seedGlobalConf(t *testing.T) {
	t.Helper()
	old := conf.Snapshot()
	conf.SetGlobalConfig(&conf.AppConfiguration{})
	t.Cleanup(func() { conf.SetGlobalConfig(old) })
}

// TestSanitizeForAdmin_NoPlaintext 管理台读响应体不含明文（issue 验收第 2 条）。
// 负对照内嵌：**未经脱敏**的原始 SystemConfig 直接序列化必须扫得到明文——
// 证明扫描手法有效、脱敏边界有牙齿（改回明文实现 ⇒ 本条当场转红）。
func TestSanitizeForAdmin_NoPlaintext(t *testing.T) {
	cfg := &SystemConfig{
		OIDC: &conf.OIDCConfig{
			Enabled:      true,
			Issuer:       "https://idp.example.com",
			ClientID:     "client-1",
			ClientSecret: conf.NewSecretString(sanitizeMarker),
		},
	}

	// 负对照：原始序列化（= 落库 wire 形状）扫得到明文。
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	if !bytes.Contains(raw, []byte(sanitizeMarker)) {
		t.Fatal("负对照失败：原始序列化扫不到明文，扫描手法本身有问题")
	}

	got := SanitizeForAdmin(cfg)
	body, err := json.Marshal(got)
	require.NoError(t, err)
	if bytes.Contains(body, []byte(sanitizeMarker)) {
		t.Fatal("管理台读响应泄漏明文 client_secret")
	}
	var m map[string]any
	require.NoError(t, json.Unmarshal(body, &m))
	oidc := m["oidc"].(map[string]any)
	assert.Equal(t, conf.SecretMask, oidc["client_secret"], "已配置 ⇒ 掩码哨兵")
	assert.Equal(t, "client-1", oidc["client_id"], "非敏感字段原样")

	// 原始配置不受脱敏影响（真值仍在，供落库/热更）。
	assert.Equal(t, sanitizeMarker, cfg.OIDC.ClientSecret.Get())
}

// TestSanitizeForAdmin_UnsetSecret 未配置（nil / 空持有器）回空串而非掩码——
// 前端密码框可区分「已配置/未配置」两态。
func TestSanitizeForAdmin_UnsetSecret(t *testing.T) {
	for name, cfg := range map[string]*SystemConfig{
		"nil_holder": {OIDC: &conf.OIDCConfig{}},
		"empty":      {OIDC: &conf.OIDCConfig{ClientSecret: conf.NewSecretString("")}},
	} {
		t.Run(name, func(t *testing.T) {
			body, err := json.Marshal(SanitizeForAdmin(cfg))
			require.NoError(t, err)
			var m map[string]any
			require.NoError(t, json.Unmarshal(body, &m))
			assert.Equal(t, "", m["oidc"].(map[string]any)["client_secret"])
		})
	}
	assert.Nil(t, SanitizeForAdmin(nil), "nil 入 nil 出")
}

// TestUpdateConfig_MaskSentinelPreservesSecret 掩码哨兵回填：提交值 == 掩码
// ⇒ 保留现值（identity 与内容都不变），掩码不落库、不进 conf。
func TestUpdateConfig_MaskSentinelPreservesSecret(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	seedGlobalConf(t)

	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		OIDC: &conf.OIDCConfig{Enabled: true, ClientSecret: conf.NewSecretString(sanitizeMarker)},
	}))
	live := conf.Snapshot().Security.OIDC.ClientSecret
	require.Equal(t, sanitizeMarker, live.Get())

	// 管理台表单原样回传掩码 → 保存 → 现值原样保留（同持有器）。
	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		OIDC: &conf.OIDCConfig{Enabled: true, ClientSecret: conf.NewSecretString(conf.SecretMask)},
	}))
	assert.Same(t, live, conf.Snapshot().Security.OIDC.ClientSecret, "哨兵回填必须复用现持有器")
	assert.Equal(t, sanitizeMarker, conf.Snapshot().Security.OIDC.ClientSecret.Get())
	if bytes.Contains(sectest.RawObfuscated(t, conf.Snapshot().Security.OIDC.ClientSecret), []byte(conf.SecretMask)) {
		t.Fatal("掩码被当成新密钥存进了持有器")
	}

	// 落库记录同样保留真值（掩码不落库）。
	persisted, err := svc.loadPersistedConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, persisted.OIDC)
	assert.Equal(t, sanitizeMarker, persisted.OIDC.ClientSecret.Get())
}

// TestUpdateConfig_RotateSecretClearsOld 热更旋转：conf 换新值，旧持有器
// 混淆存储零化（反射断言，issue 验收第 4 条）。
func TestUpdateConfig_RotateSecretClearsOld(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	seedGlobalConf(t)

	old := conf.NewSecretString("rotate-old-2b71")
	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		OIDC: &conf.OIDCConfig{Enabled: true, ClientSecret: old},
	}))
	require.Equal(t, "rotate-old-2b71", conf.Snapshot().Security.OIDC.ClientSecret.Get())

	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		OIDC: &conf.OIDCConfig{Enabled: true, ClientSecret: conf.NewSecretString("rotate-new-9d44")},
	}))
	cur := conf.Snapshot().Security.OIDC.ClientSecret
	require.NotNil(t, cur)
	assert.Equal(t, "rotate-new-9d44", cur.Get(), "conf 即刻生效新值")
	assert.NotSame(t, old, cur)
	sectest.AssertCleared(t, old)

	// 镜像（管理台回读底座）同步为新值；落库为提交意图（新明文）。
	got, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	assert.Equal(t, "rotate-new-9d44", got.OIDC.ClientSecret.Get())
	persisted, err := svc.loadPersistedConfig(ctx)
	require.NoError(t, err)
	assert.Equal(t, "rotate-new-9d44", persisted.OIDC.ClientSecret.Get())
}

// TestUpdateConfig_EqualValueReusesHolder 值等复用：提交与现值明文相同的
// 保存复用现持有器（identity 稳定 ⇒ overlay 守卫跳过 Clear ⇒ 未改 secret 的
// 保存不零化现役密钥、不毒化在途请求）。
func TestUpdateConfig_EqualValueReusesHolder(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	seedGlobalConf(t)

	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		OIDC: &conf.OIDCConfig{Enabled: true, ClientSecret: conf.NewSecretString(sanitizeMarker)},
	}))
	live := conf.Snapshot().Security.OIDC.ClientSecret

	// 值相同（新持有器、同明文）→ 复用现持有器。
	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		OIDC: &conf.OIDCConfig{Enabled: true, ClientSecret: conf.NewSecretString(sanitizeMarker)},
	}))
	assert.Same(t, live, conf.Snapshot().Security.OIDC.ClientSecret)
	sectest.AssertNotStoredInPlaintext(t, conf.Snapshot().Security.OIDC.ClientSecret, sanitizeMarker)
}

// TestUpdateConfigOIDCSectionAbsentKeepsSecret 携带其它段的局部保存不冲掉
// 已配置的 client_secret（nil 段保留 + 持有器随 conf 原样）。
func TestUpdateConfigOIDCSectionAbsentKeepsSecret(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	seedGlobalConf(t)

	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		OIDC: &conf.OIDCConfig{Enabled: true, ClientSecret: conf.NewSecretString(sanitizeMarker)},
	}))
	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		Base: &BaseConfig{Name: "renamed", Description: "d"},
	}))
	got, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, got.OIDC)
	require.NotNil(t, got.OIDC.ClientSecret)
	assert.True(t, got.OIDC.Enabled)
	assert.Equal(t, sanitizeMarker, got.OIDC.ClientSecret.Get())
}
