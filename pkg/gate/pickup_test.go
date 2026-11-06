package gate

import (
	"context"
	"testing"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withLockout 设定锁定阈值并重建全局锁定器（内存模式，表随重建清空），
// 保证用例间失败计数互不串扰。
func withLockout(t *testing.T, maxAttempts int) {
	t.Helper()
	cfg := &conf.AppConfiguration{}
	cfg.Security.Lockout = conf.LockoutConfig{
		Enabled: true, MaxAttempts: maxAttempts, WindowSeconds: 300, LockSeconds: 600,
	}
	old := conf.GetGlobalConfig()
	conf.SetGlobalConfig(cfg)
	middleware.InitDefaultLockout(nil)
	t.Cleanup(func() {
		conf.SetGlobalConfig(old)
		middleware.InitDefaultLockout(nil)
	})
}

func mustHash(t *testing.T, pw string) string {
	t.Helper()
	h, err := utils.HashPassword(pw)
	require.NoError(t, err)
	return h
}

func pickup(st PickupState, req PickupCheck) PickupDecision {
	return CheckPickup(context.Background(), st, req)
}

// TestCheckPickup_VerdictOrder 判定顺序固定：过期 → 管控 → 就绪 → 密码。
func TestCheckPickup_VerdictOrder(t *testing.T) {
	withLockout(t, 10)
	base := PickupCheck{ClientIP: "1.2.3.4", Code: "ABC123", Password: ""}

	d := pickup(PickupState{Expired: true, Blocked: true, NotReady: true, RequireAuth: true}, base)
	assert.Equal(t, PickupUnavailable, d.Verdict, "过期优先于一切")

	d = pickup(PickupState{Blocked: true, BlockedStatus: "pending_review", NotReady: true, RequireAuth: true}, base)
	assert.Equal(t, PickupBlocked, d.Verdict)
	assert.Equal(t, "pending_review", d.BlockedStatus)

	d = pickup(PickupState{NotReady: true, RequireAuth: true}, base)
	assert.Equal(t, PickupNotReady, d.Verdict)
}

// TestCheckPickup_NoAuthAndTokenBypass 无密码分享直接放行；持下载令牌跳过密码段。
func TestCheckPickup_NoAuthAndTokenBypass(t *testing.T) {
	withLockout(t, 10)
	d := pickup(PickupState{}, PickupCheck{ClientIP: "1.2.3.4", Code: "X"})
	assert.Equal(t, PickupAllowed, d.Verdict)

	d = pickup(PickupState{RequireAuth: true, PasswordHash: mustHash(t, "pw")},
		PickupCheck{ClientIP: "1.2.3.4", Code: "X", AuthedByToken: true})
	assert.Equal(t, PickupAllowed, d.Verdict, "令牌等价已认证，空密码也放行")
}

// TestCheckPickup_MissingPasswordSemantics 空密码：MissingCountsFailure=false
// 是提示态（不记账），=true 按失败记账（对齐 preview/anonymous/download 历史规格）。
func TestCheckPickup_MissingPasswordSemantics(t *testing.T) {
	withLockout(t, 10)
	st := PickupState{RequireAuth: true, PasswordHash: mustHash(t, "pw")}

	d := pickup(st, PickupCheck{ClientIP: "1.2.3.4", Code: "M1", Password: ""})
	assert.Equal(t, PickupPasswordMissing, d.Verdict)

	d = pickup(st, PickupCheck{ClientIP: "1.2.3.4", Code: "M2", Password: "", MissingCountsFailure: true})
	assert.Equal(t, PickupPasswordWrong, d.Verdict)
}

// TestCheckPickup_WrongRightPassword 密码错误记账、正确放行并清零计数。
func TestCheckPickup_WrongRightPassword(t *testing.T) {
	withLockout(t, 10)
	st := PickupState{RequireAuth: true, PasswordHash: mustHash(t, "right")}
	req := PickupCheck{ClientIP: "1.2.3.4", Code: "PW1", Password: "wrong"}

	assert.Equal(t, PickupPasswordWrong, pickup(st, req).Verdict)

	req.Password = "right"
	assert.Equal(t, PickupAllowed, pickup(st, req).Verdict)

	// 脏数据：require_auth=true 而哈希缺失 → 按密码错误拒绝（不得放行）
	dirty := PickupState{RequireAuth: true, PasswordHash: ""}
	req.Password = "anything"
	assert.Equal(t, PickupPasswordWrong, pickup(dirty, req).Verdict)
}

// TestCheckPickup_LockoutSharedAcrossEndpoints 达阈值触发锁定；锁定判定先于
// 密码校验（正确密码也拒）；scope 统一使"换入口"不再重置爆破预算。
func TestCheckPickup_LockoutSharedAcrossEndpoints(t *testing.T) {
	withLockout(t, 2)
	st := PickupState{RequireAuth: true, PasswordHash: mustHash(t, "right")}
	wrong := PickupCheck{ClientIP: "9.9.9.9", Code: "LOCK1", Password: "bad"}

	assert.Equal(t, PickupPasswordWrong, pickup(st, wrong).Verdict)
	assert.Equal(t, PickupPasswordWrong, pickup(st, wrong).Verdict) // 达到阈值 2

	d := pickup(st, wrong)
	assert.Equal(t, PickupLocked, d.Verdict, "第三次应先撞锁定")
	assert.Greater(t, d.LockRemain, 0)

	right := PickupCheck{ClientIP: "9.9.9.9", Code: "LOCK1", Password: "right"}
	assert.Equal(t, PickupLocked, pickup(st, right).Verdict, "锁定期内正确密码也拒")

	// 不同 (IP, code) 维度互不影响
	other := PickupCheck{ClientIP: "9.9.9.8", Code: "LOCK1", Password: "right"}
	assert.Equal(t, PickupAllowed, pickup(st, other).Verdict)
}

// TestPickupLockKeyHelpers 枚举记账必须落在**按 IP** 的维度上。
//
// 回归锁（2026-11-06 P2）：既有记账维度是 (IP, 码)，而枚举攻击的特征就是**每次
// 换一个码** ⇒ 每个码各有一份计数、谁也到不了阈值 ⇒ 记了等于没记。本用例的
// 核心断言是「同一 IP 连续试**不同**码也累计」——旧实现下这条必然失败。
func TestPickupLockKeyHelpers(t *testing.T) {
	withLockout(t, 2)
	ctx := context.Background()

	_, locked := PickupEnumLockedRemain(ctx, "5.5.5.5")
	assert.False(t, locked, "未记账前不得锁定")

	RecordPickupFailure(ctx, "5.5.5.5", "ENUM1")
	RecordPickupFailure(ctx, "5.5.5.5", "ENUM2") // 故意换码：枚举的典型形态
	remain, locked := PickupEnumLockedRemain(ctx, "5.5.5.5")
	assert.True(t, locked, "同一 IP 换码累计达阈值后必须锁定（否则枚举无防护）")
	assert.Greater(t, remain, 0)

	// 换码也逃不掉：入口预检（含枚举维度）对**任意**码都返回锁定
	if _, locked := PickupLockedRemain(ctx, "5.5.5.5", "ENUM3"); !locked {
		t.Fatal("PickupLockedRemain 必须并入按 IP 的枚举维度")
	}

	// 码维度自身保持独立（未失败过的码不在码维度上被锁）——这是与旧行为一致的半边
	lock := middleware.GetDefaultLockout()
	if _, hit := lock.CheckLocked(ctx, PickupLockKey("5.5.5.5", "ENUM3")); hit {
		t.Fatal("未失败过的码不应在码维度上被锁（两个维度不得混为一谈）")
	}

	// 枚举锁定按 IP 隔离，不牵连他人
	if _, locked := PickupEnumLockedRemain(ctx, "6.6.6.6"); locked {
		t.Fatal("枚举锁定必须按 IP 隔离")
	}

	// 空 IP 不记枚举维度：取不到 IP 的来源会挤进同一个键，锁一个等于锁全网
	RecordPickupFailure(ctx, "", "ENUM4")
	RecordPickupFailure(ctx, "", "ENUM5")
	if _, locked := PickupEnumLockedRemain(ctx, ""); locked {
		t.Fatal("空 IP 不得进入枚举维度")
	}
}

// TestCheckPickup_EnumLockoutBlocksPasswordlessShare 枚举锁定必须拦住**无密码**
// 分享。枚举者命中的多是无需密码的公开分享；若这道闸排在「是否需要密码」之后，
// 无密码分享的枚举会整条绕过去（本用例即锁住闸门位置）。
func TestCheckPickup_EnumLockoutBlocksPasswordlessShare(t *testing.T) {
	withLockout(t, 2)
	ctx := context.Background()
	RecordPickupFailure(ctx, "7.7.7.7", "E1")
	RecordPickupFailure(ctx, "7.7.7.7", "E2")

	d := pickup(PickupState{}, PickupCheck{ClientIP: "7.7.7.7", Code: "E3"})
	assert.Equal(t, PickupLocked, d.Verdict, "枚举已达阈值：无密码分享也必须先撞锁定")
	assert.Greater(t, d.LockRemain, 0)

	// 负对照：另一个 IP 正常放行（维度是 IP，不是全局开关）
	assert.Equal(t, PickupAllowed, pickup(PickupState{}, PickupCheck{ClientIP: "8.8.8.8", Code: "E3"}).Verdict)
}

// TestCheckPickup_SuccessResetsEnumCounter 成功取件（凭据为真，不是猜出来的）
// 必须清零枚举计数，否则历史误输会把后续正常取件连坐。
func TestCheckPickup_SuccessResetsEnumCounter(t *testing.T) {
	withLockout(t, 3)
	ctx := context.Background()
	st := PickupState{RequireAuth: true, PasswordHash: mustHash(t, "right")}
	RecordPickupFailure(ctx, "9.1.1.1", "S1")
	RecordPickupFailure(ctx, "9.1.1.1", "S2")

	assert.Equal(t, PickupAllowed,
		pickup(st, PickupCheck{ClientIP: "9.1.1.1", Code: "S3", Password: "right"}).Verdict)

	if _, locked := PickupEnumLockedRemain(ctx, "9.1.1.1"); locked {
		t.Fatal("成功取件后枚举计数必须清零")
	}
	// 清零后可再失败 MaxAttempts 次才锁
	RecordPickupFailure(ctx, "9.1.1.1", "S4")
	RecordPickupFailure(ctx, "9.1.1.1", "S5")
	if _, locked := PickupEnumLockedRemain(ctx, "9.1.1.1"); locked {
		t.Fatal("清零后两次失败不应立刻锁定（阈值 3）")
	}
}
