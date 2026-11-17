// Package l8 是「L8 安全测试层」的专属测试包。
//
// 为什么单独成包：L8 的用例横跨多个域（gate/middleware/auth/security/utils），
// 而 pkg/gate、pkg/bootstrap 等目录正被其它会话占用（并行开发时不可写）。
// 本包只放**跨域**的 L8 用例（执行点级别的判定），域内用例仍留在各自包里
// （pkg/auth、pkg/middleware、pkg/security、pkg/utils、transport/http/middleware）。
//
// 矩阵真相源：pkg/l8/matrix.json（机器可读清单）。
// 门禁：scripts/check-l8-security-matrix.sh（清单 ↔ 仓内测试双向对账）。
package l8

// 本文件覆盖 L8 · resource_exhaustion 与 L8 · pickup_enum 的**执行点**判定：
//
//   - gate.BatchCapBelowFileCap：AGENTS 硬规则「总量 ≥ 单文件」的反不变量判定，
//     此前 0 覆盖 ⇒ 出厂默认自相矛盾（2026-10-30 事故同款）不会被任何测试拦下。
//   - gate.CheckPresignPolicy：直传三档策略闸门（everyone / authenticated /
//     disabled），此前 0 覆盖 ⇒ 关闭直传后匿名仍能直传不会被测试发现。
//   - gate.PrecheckPickup：所有公开取件入口的统一预检（枚举/爆破锁定），
//     此前 0 覆盖（只有 handler 侧行为级用例间接走到）。

import (
	"context"
	"testing"

	"github.com/pigeonbox/contracts/errcode"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/gate"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// l8WithConf 替换全局配置并在用例结束后复位（连带重建全局锁定器）。
func l8WithConf(t *testing.T, cfg *conf.AppConfiguration) {
	t.Helper()
	old := conf.GetGlobalConfig()
	conf.SetGlobalConfig(cfg)
	middleware.InitDefaultLockout(nil)
	t.Cleanup(func() {
		conf.SetGlobalConfig(old)
		middleware.InitDefaultLockout(nil)
	})
}

// l8WithLockout 设定锁定阈值并重建全局锁定器（内存模式，计数随重建清空）。
func l8WithLockout(t *testing.T, maxAttempts int) {
	t.Helper()
	cfg := &conf.AppConfiguration{}
	cfg.Security.Lockout = conf.LockoutConfig{
		Enabled: true, MaxAttempts: maxAttempts, WindowSeconds: 300, LockSeconds: 600,
	}
	l8WithConf(t, cfg)
}

// TestL8_BatchCapBelowFileCapDetectsContradiction 「总量 < 单文件」必须被判为矛盾。
//
// 语义：upload.max_file_size（单文件）与 user.user_upload_size（单次发送总量）
// 是两个旋钮；总量小于单文件时，超大文件永远发不出去（先被总量闸拒），
// 而 UI 仍在宣称单文件上限有效 ⇒ 2026-10-30 出厂默认自相矛盾事故。
// 0 = 不限，不构成矛盾。
func TestL8_BatchCapBelowFileCapDetectsContradiction(t *testing.T) {
	// 矛盾形态：总量 100 < 单文件 200
	assert.True(t, gate.BatchCapBelowFileCap(100, 200), "总量 < 单文件 必须判为矛盾")
	assert.True(t, gate.BatchCapBelowFileCap(1, 2))

	// 正常形态
	assert.False(t, gate.BatchCapBelowFileCap(200, 100), "总量 ≥ 单文件 不构成矛盾")
	assert.False(t, gate.BatchCapBelowFileCap(100, 100), "总量 = 单文件 不构成矛盾")

	// 0 = 不限：任一为 0 都不构成矛盾
	assert.False(t, gate.BatchCapBelowFileCap(0, 200), "总量不限 ⇒ 不构成矛盾")
	assert.False(t, gate.BatchCapBelowFileCap(100, 0), "单文件不限 ⇒ 不构成矛盾")
	assert.False(t, gate.BatchCapBelowFileCap(0, 0))
}

// TestL8_CheckPresignPolicyGate 直传三档策略闸门。
func TestL8_CheckPresignPolicyGate(t *testing.T) {
	userID := uint(7)

	t.Run("everyone 放行匿名", func(t *testing.T) {
		cfg := &conf.AppConfiguration{}
		cfg.Download.PresignPolicy = conf.PresignPolicyEveryone
		l8WithConf(t, cfg)
		require.NoError(t, gate.CheckPresignPolicy(nil))
		require.NoError(t, gate.CheckPresignPolicy(&userID))
	})

	t.Run("authenticated 拒匿名放行登录", func(t *testing.T) {
		cfg := &conf.AppConfiguration{}
		cfg.Download.PresignPolicy = conf.PresignPolicyAuthenticated
		l8WithConf(t, cfg)

		err := gate.CheckPresignPolicy(nil)
		require.Error(t, err, "authenticated 策略下匿名必须被拒")
		assert.ErrorIs(t, err, gate.ErrPresignDisabled)

		require.NoError(t, gate.CheckPresignPolicy(&userID), "登录用户不受影响")
	})

	t.Run("disabled 全员拒", func(t *testing.T) {
		cfg := &conf.AppConfiguration{}
		cfg.Download.PresignPolicy = conf.PresignPolicyDisabled
		l8WithConf(t, cfg)

		err := gate.CheckPresignPolicy(nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, gate.ErrPresignDisabled)

		err = gate.CheckPresignPolicy(&userID)
		require.Error(t, err, "disabled 是总闸：登录用户也拒（密钥轮换/通道故障时的兜底）")
		assert.ErrorIs(t, err, gate.ErrPresignDisabled)
	})

	t.Run("未配置按 everyone 处理", func(t *testing.T) {
		l8WithConf(t, &conf.AppConfiguration{})
		require.NoError(t, gate.CheckPresignPolicy(nil))
	})

	t.Run("拒绝携带业务码", func(t *testing.T) {
		cfg := &conf.AppConfiguration{}
		cfg.Download.PresignPolicy = conf.PresignPolicyDisabled
		l8WithConf(t, cfg)

		err := gate.CheckPresignPolicy(nil)
		require.Error(t, err)
		type coder interface{ ErrCode() int }
		ce, ok := err.(coder)
		require.True(t, ok, "闸门拒绝必须携带业务码（否则前端拿不到 10015）")
		assert.Equal(t, errcode.CodePresignDisabled, ce.ErrCode())
	})
}

// TestL8_PrecheckPickupBlocksLockedCaller 取件入口预检：被锁定 ⇒ 429 型错误。
func TestL8_PrecheckPickupBlocksLockedCaller(t *testing.T) {
	l8WithLockout(t, 2)
	ctx := context.Background()
	ip := "10.0.0.9"

	// 未锁定 ⇒ 放行
	require.NoError(t, gate.PrecheckPickup(ctx, ip, "ABC123"))

	// 两次失败（枚举维度按 IP 累计）⇒ 锁定
	gate.RecordPickupFailure(ctx, ip, "AAA111")
	gate.RecordPickupFailure(ctx, ip, "BBB222")

	err := gate.PrecheckPickup(ctx, ip, "CCC333")
	require.Error(t, err, "枚举已达阈值 ⇒ 任意码都必须被预检拦下")

	lockedErr, ok := err.(*gate.PickupLockedError)
	require.True(t, ok, "预检失败必须是 PickupLockedError（HTTP 层据此回 429）")
	assert.Greater(t, lockedErr.Remain, 0)
	assert.Contains(t, lockedErr.Error(), "秒后重试", "文案口径须与历史一致")

	// 另一个 IP 不受影响（锁定按 IP 隔离）
	require.NoError(t, gate.PrecheckPickup(ctx, "10.0.0.10", "ABC123"))
}

// TestL8_PrecheckPickupEmptyIPDoesNotLockEveryone 空 IP 不得进入枚举维度。
//
// 取不到 IP 的来源（无代理头、直连）会共用同一个键 ⇒ 锁一个等于锁全网。
func TestL8_PrecheckPickupEmptyIPDoesNotLockEveryone(t *testing.T) {
	l8WithLockout(t, 2)
	ctx := context.Background()

	gate.RecordPickupFailure(ctx, "", "AAA111")
	gate.RecordPickupFailure(ctx, "", "BBB222")

	_, locked := gate.PickupEnumLockedRemain(ctx, "")
	assert.False(t, locked, "空 IP 不得进入枚举维度")
}
