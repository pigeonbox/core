package share

import (
	"context"
	"testing"

	"github.com/pigeonbox/core/pkg/security"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// modelFileCode 造一条分享记录（测试用）。
func modelFileCode(code string, count int) model.FileCode {
	return model.FileCode{Code: code, FilePath: "uploads", UUIDFileName: "a.bin", ExpiredCount: count}
}

// SetTestSecret 注入计次键用的密钥（单向摘要的盐）。
func SetTestSecret(t *testing.T) {
	t.Helper()
	security.SetDownloadTokenSecret("test-secret-32-chars-xxxxxxxxxxxxx")
}

// 文本交付面的计次键 = (分享码, 取件人 IP)：
//   - 同一 IP 重复打开 ⇒ 同一个键 ⇒ 只扣 1 次（刷新页面不重复吃额度）；
//   - 不同 IP ⇒ 各自的键 ⇒ 各扣 1 次（owner 的「N 次」= N 个不同取件者）。
func TestTextViewKey_PerViewer(t *testing.T) {
	SetTestSecret(t)

	k1 := security.TextViewKey("CODE01", "10.0.0.1")
	k2 := security.TextViewKey("CODE01", "10.0.0.2")
	k1again := security.TextViewKey("CODE01", "10.0.0.1")
	other := security.TextViewKey("CODE02", "10.0.0.1")

	assert.NotEmpty(t, k1)
	assert.Equal(t, k1, k1again, "同一取件人对同一分享 ⇒ 同一个计次键")
	assert.NotEqual(t, k1, k2, "不同取件人 ⇒ 不同计次键")
	assert.NotEqual(t, k1, other, "不同分享 ⇒ 不同计次键")
	// 键里不得含明文 IP（落库的是单向摘要）
	assert.NotContains(t, k1, "10.0.0.1")
}

// 不同取件人各扣 1 次；同一取件人重复交付只扣 1 次。
func TestChargePickup_TextPerViewer(t *testing.T) {
	svc, g, _, _, _ := newTestServiceWithDB(t)
	ctx := context.Background()
	rec := modelFileCode("TXTCHG", 3)
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &rec))

	// 取件人 A 第一次
	charged, exhausted, err := svc.ChargePickup(ctx, "TXTCHG",
		security.TextViewKey("TXTCHG", "10.0.0.1"), "select-text", "10.0.0.1")
	require.NoError(t, err)
	assert.True(t, charged, "取件人 A 首次应扣次")
	assert.False(t, exhausted)

	// 取件人 A 重复打开 ⇒ 不再扣
	charged, exhausted, err = svc.ChargePickup(ctx, "TXTCHG",
		security.TextViewKey("TXTCHG", "10.0.0.1"), "select-text", "10.0.0.1")
	require.NoError(t, err)
	assert.False(t, charged, "同一取件人重复打开不重复扣次")
	assert.False(t, exhausted, "已取过的人不该被判「次数用完」")

	// 取件人 B ⇒ 扣第 2 次
	charged, exhausted, err = svc.ChargePickup(ctx, "TXTCHG",
		security.TextViewKey("TXTCHG", "10.0.0.2"), "select-text", "10.0.0.2")
	require.NoError(t, err)
	assert.True(t, charged, "取件人 B 首次应扣次")
	assert.False(t, exhausted)

	// 取件人 C ⇒ 扣第 3 次（额度=3）
	charged, exhausted, err = svc.ChargePickup(ctx, "TXTCHG",
		security.TextViewKey("TXTCHG", "10.0.0.3"), "select-text", "10.0.0.3")
	require.NoError(t, err)
	assert.True(t, charged)
	assert.False(t, exhausted)

	// 取件人 D ⇒ 额度用完 ⇒ exhausted
	charged, exhausted, err = svc.ChargePickup(ctx, "TXTCHG",
		security.TextViewKey("TXTCHG", "10.0.0.4"), "select-text", "10.0.0.4")
	require.NoError(t, err)
	assert.False(t, charged)
	assert.True(t, exhausted, "额度用完 ⇒ 拒绝新的取件")

	// 已取过的人（A）仍能打开（不重复扣、也不算耗尽）
	charged, exhausted, err = svc.ChargePickup(ctx, "TXTCHG",
		security.TextViewKey("TXTCHG", "10.0.0.1"), "select-text", "10.0.0.1")
	require.NoError(t, err)
	assert.False(t, charged)
	assert.False(t, exhausted, "已取过的人不该被判「次数用完」")

	fc, err := svc.fileCodeRepo.GetByCode(ctx, "TXTCHG")
	require.NoError(t, err)
	assert.Equal(t, 0, fc.ExpiredCount, "3 个取件人扣了 3 次")
	assert.Equal(t, 3, fc.UsedCount)
	_ = g
}
