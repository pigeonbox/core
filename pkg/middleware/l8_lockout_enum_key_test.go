package middleware

// L8 · pickup_enum / resource_exhaustion：失败锁定键的大小写折叠与「锁定器关闭」语义。
//
// 威胁 1（变体绕过）：取件码/分享码查询默认折叠大小写（download.code_case_insensitive）。
// 若锁定键不折叠，攻击者用 "abc123" / "ABC123" 轮换试探，同一 (IP, 码) 的失败
// 计数永远到不了阈值 ⇒ 爆破/枚举防护形同虚设。LookupLockKey 是这个折叠的唯一
// 实现点，此前 0 覆盖（只有 pkg/gate 的用例间接调用，覆盖率口径上看不到）。
//
// 威胁 2（关掉即失效）：security.lockout.enabled=false 时锁定器整体关闭
// （CheckLocked 恒未锁、RecordFailure 不计数）。这是运维显式配置的语义，
// 但此前 Enabled() 分支 0 覆盖 ⇒ 判据不在这儿，容易被误读成"仍然生效"。

import (
	"context"
	"testing"

	"github.com/pigeonbox/core/conf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// l8WithConf 替换全局配置并在用例结束后复位。
func l8WithConf(t *testing.T, cfg *conf.AppConfiguration) {
	t.Helper()
	old := conf.GetGlobalConfig()
	conf.SetGlobalConfig(cfg)
	t.Cleanup(func() { conf.SetGlobalConfig(old) })
}

// l8FoldOff 构造「大小写不折叠」的配置。
func l8FoldOff() *conf.AppConfiguration {
	cfg := &conf.AppConfiguration{}
	off := false
	cfg.Download.CodeCaseInsensitive = &off
	return cfg
}

// TestL8_LookupLockKeyFoldsCase 折叠开启时，大小写变体共用同一把失败计数。
func TestL8_LookupLockKeyFoldsCase(t *testing.T) {
	l8WithConf(t, &conf.AppConfiguration{}) // nil 配置 ⇒ 折叠默认开

	lower := LookupLockKey("pickup", "1.2.3.4", "abc123")
	upper := LookupLockKey("pickup", "1.2.3.4", "ABC123")
	require.NotEmpty(t, lower)
	assert.Equal(t, lower, upper,
		"大小写变体必须共用同一失败计数键，否则变体轮换可绕过爆破锁定")
}

// TestL8_LookupLockKeySeparatesDimensions 键必须按 scope/IP/码三个维度区分。
func TestL8_LookupLockKeySeparatesDimensions(t *testing.T) {
	l8WithConf(t, &conf.AppConfiguration{})

	base := LookupLockKey("pickup", "1.2.3.4", "abc123")
	assert.NotEqual(t, base, LookupLockKey("pickup-enum", "1.2.3.4", "abc123"), "scope 维度必须区分")
	assert.NotEqual(t, base, LookupLockKey("pickup", "5.6.7.8", "abc123"), "IP 维度必须区分")
	assert.NotEqual(t, base, LookupLockKey("pickup", "1.2.3.4", "xyz789"), "码维度必须区分")
}

// TestL8_LookupLockKeyRespectsFoldSwitch 显式关闭折叠时，大小写变体不再共用计数。
//
// 这是配置语义（运维主动选择区分大小写），但必须与折叠开启时的行为不同，
// 否则改配置形同虚设。
func TestL8_LookupLockKeyRespectsFoldSwitch(t *testing.T) {
	l8WithConf(t, l8FoldOff())

	lower := LookupLockKey("pickup", "1.2.3.4", "abc123")
	upper := LookupLockKey("pickup", "1.2.3.4", "ABC123")
	require.NotEmpty(t, lower)
	assert.NotEqual(t, lower, upper, "关闭折叠后大小写必须是不同的计数键")
}

// TestL8_LockoutDisabledFailsOpenByConfig 锁定器被显式关闭时不计数、不锁定。
//
// 语义来源：conf.Security.Lockout.Enabled=false（运维显式关闭爆破防护）。
// 判据钉住"关闭即关闭"——防的是有人以为关掉配置仍会锁（误判运维预期）。
func TestL8_LockoutDisabledFailsOpenByConfig(t *testing.T) {
	cfg := &conf.AppConfiguration{}
	cfg.Security.Lockout = conf.LockoutConfig{
		Enabled: false, MaxAttempts: 10, WindowSeconds: 300, LockSeconds: 600,
	}
	l8WithConf(t, cfg)

	lo := NewLockout(nil)
	require.NotNil(t, lo)
	assert.False(t, lo.Enabled(), "配置关闭时锁定器必须报告未启用")

	ctx := context.Background()
	remain, locked := lo.CheckLocked(ctx, "k1")
	assert.False(t, locked)
	assert.Equal(t, 0, remain)

	triggered, secs := lo.RecordFailure(ctx, "k1")
	assert.False(t, triggered, "关闭状态不得触发锁定")
	assert.Equal(t, 0, secs)
}

// TestL8_LockoutEnabledByDefault 配置缺省时锁定器是启用的（防"默认失效"回归）。
func TestL8_LockoutEnabledByDefault(t *testing.T) {
	l8WithConf(t, &conf.AppConfiguration{})

	lo := NewLockout(nil)
	require.NotNil(t, lo)
	assert.True(t, lo.Enabled(), "未显式关闭时锁定器必须启用")

	// 默认阈值 10 次 ⇒ 第 10 次失败触发锁定
	ctx := context.Background()
	for i := 0; i < 9; i++ {
		triggered, _ := lo.RecordFailure(ctx, "k-default")
		assert.False(t, triggered, "第 %d 次失败不应触发锁定", i+1)
	}
	triggered, secs := lo.RecordFailure(ctx, "k-default")
	assert.True(t, triggered, "第 10 次失败必须触发锁定")
	assert.Greater(t, secs, 0)

	remain, locked := lo.CheckLocked(ctx, "k-default")
	assert.True(t, locked)
	assert.Greater(t, remain, 0)
}
