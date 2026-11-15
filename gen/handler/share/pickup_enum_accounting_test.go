// core#182 取件枚举记账口径：只有「码确证不存在」才入枚举锁定桶。
//
// 此前 gen/handler/share 的 GetShare / authorizeDownload 对 GetFileByCode 的
// **非封禁错误全记**——过期（validateShareRecord 的 "file has expired"）与
// 基建故障（DB 不可达）也计入 (IP,码) + 按 IP 双维度枚举锁定桶：
//
//   - 正常用户反复刷新已过期的分享页 ⇒ 撞锁 429「尝试过于频繁」；
//   - DB 故障期全网请求被记成枚举攻击 ⇒ 故障被放大成全网锁定。
//
// metadata（transport/http/handler/share_metadata.go）与 preview（app/preview）
// 两面早已用 gorm.ErrRecordNotFound 精确排除（core#163），本面与之对齐：
// 只有 errors.Is(err, shareService.ErrShareGone) 才记账。
//
// 断言必须能变红：把 handler 的 ErrShareGone 判据换回「非封禁全记」，
// 过期/基建两条用例立刻红（阈值 3、请求 4 次 ⇒ 桶被记满）。
package share

import (
	"context"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/gate"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
)

// withPickupLockout 设失败锁定阈值并重建全局锁定器（内存计数随重建清空），
// 保证用例间计数不串扰（与 pkg/gate 测试的 withLockout 同规格）。
// 必须在 newChargeTestEnv 之后调用（env 会整体替换全局配置）。
func withPickupLockout(t *testing.T, maxAttempts int) {
	t.Helper()
	cfg := conf.GetGlobalConfig()
	if cfg == nil {
		cfg = &conf.AppConfiguration{}
	}
	old := cfg.Security.Lockout
	cfg.Security.Lockout = conf.LockoutConfig{
		Enabled: true, MaxAttempts: maxAttempts, WindowSeconds: 300, LockSeconds: 600,
	}
	conf.SetGlobalConfig(cfg)
	middleware.InitDefaultLockout(nil)
	t.Cleanup(func() {
		cfg.Security.Lockout = old
		conf.SetGlobalConfig(cfg)
		middleware.InitDefaultLockout(nil)
	})
}

// pickupBucketLocked 枚举桶是否已锁定。
//
// handler 内 ClientIP 在 ut 请求环境下的取值：ut 不设 conn ⇒ hertz 返回
// zeroTCPAddr（"0.0.0.0:0"）⇒ ResolveClientIP 归一为 "0.0.0.0"。用与 handler
// 同一条解析链算出主候选，另留候选兜口径漂移；任一候选命中即「桶被记满」。
func pickupBucketLocked(ctx context.Context, code string) bool {
	candidates := []string{
		middleware.ResolveClientIP("0.0.0.0:0", ""),
		"unknown", "127.0.0.1", "",
	}
	for _, ip := range candidates {
		if _, locked := gate.PickupLockedRemain(ctx, ip, code); locked {
			return true
		}
	}
	return false
}

// TestGetShare_ExpiredShare_NotRecordedIntoEnumLockBucket 过期分享不得入枚举桶：
// 用户刷新失效取件页是正常行为，不是猜码。
func TestGetShare_ExpiredShare_NotRecordedIntoEnumLockBucket(t *testing.T) {
	g := newChargeTestEnv(t, true)
	withPickupLockout(t, 3)
	ctx := t.Context()

	past := time.Now().Add(-time.Hour)
	repo := dao.NewFileCodeRepository(g)
	require.NoError(t, repo.Create(ctx, &model.FileCode{
		Code: "EXPXPIRE", Text: "x", FilePath: "uploads", UUIDFileName: "x.bin",
		Size: 1, ExpiredCount: -1, ExpiredAt: &past,
	}))

	for i := 0; i < 4; i++ {
		status, body := performGet(t, "/share/select/", "code=EXPXPIRE",
			ut.Header{Key: "Host", Value: "test.local"})
		require.Equal(t, 404, status, "过期分享应 404：%s", body)
		require.Contains(t, body, "分享不存在或已过期")
	}

	assert.False(t, pickupBucketLocked(ctx, "EXPXPIRE"),
		"过期分享不得入枚举锁定桶：刷新失效页的正常用户不该被锁（core#182）")
}

// TestDownload_ExpiredShare_NotRecordedIntoEnumLockBucket 下载面同口径（authorizeDownload）。
func TestDownload_ExpiredShare_NotRecordedIntoEnumLockBucket(t *testing.T) {
	g := newChargeTestEnv(t, true)
	withPickupLockout(t, 3)
	ctx := t.Context()

	past := time.Now().Add(-time.Hour)
	repo := dao.NewFileCodeRepository(g)
	require.NoError(t, repo.Create(ctx, &model.FileCode{
		Code: "EXPDL001", Text: "x", FilePath: "uploads", UUIDFileName: "x.bin",
		Size: 1, ExpiredCount: -1, ExpiredAt: &past,
	}))

	for i := 0; i < 4; i++ {
		status, body := performDownloadPath(t, "code=EXPDL001")
		require.Equal(t, 404, status, "过期分享下载应 404（记满前不得先撞 429）：%s", body)
	}

	assert.False(t, pickupBucketLocked(ctx, "EXPDL001"),
		"下载面：过期分享不得入枚举锁定桶（core#182）")
}

// TestGetShare_InfraFailure_NotRecordedIntoEnumLockBucket DB 故障不得入枚举桶：
// 故障期把全网记成枚举攻击 = 把故障放大成全网锁定。
func TestGetShare_InfraFailure_NotRecordedIntoEnumLockBucket(t *testing.T) {
	g := newChargeTestEnv(t, true)
	withPickupLockout(t, 3)
	ctx := t.Context()

	sqlDB, err := g.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close(), "先关库以构造基建故障")

	for i := 0; i < 4; i++ {
		performGet(t, "/share/select/", "code=DBFAIL01",
			ut.Header{Key: "Host", Value: "test.local"})
	}

	assert.False(t, pickupBucketLocked(ctx, "DBFAIL01"),
		"基建故障不得入枚举锁定桶（core#182）")
}

// TestGetShare_CodeNotFound_StillRecordedIntoEnumLockBucket 负对照（必须保持绿）：
// 码**确证不存在**必须继续记账——本修复不得削弱枚举防护。
func TestGetShare_CodeNotFound_StillRecordedIntoEnumLockBucket(t *testing.T) {
	newChargeTestEnv(t, true)
	withPickupLockout(t, 3)
	ctx := t.Context()

	for i := 0; i < 3; i++ {
		status, body := performGet(t, "/share/select/", "code=NOPE0001",
			ut.Header{Key: "Host", Value: "test.local"})
		require.Equal(t, 404, status, "不存在的码应 404：%s", body)
	}

	assert.True(t, pickupBucketLocked(ctx, "NOPE0001"),
		"码确证不存在必须继续记账（枚举防护不得被本修复削弱）")
}

// TestDownload_CodeNotFound_StillRecordedIntoEnumLockBucket 下载面的负对照。
func TestDownload_CodeNotFound_StillRecordedIntoEnumLockBucket(t *testing.T) {
	newChargeTestEnv(t, true)
	withPickupLockout(t, 3)
	ctx := t.Context()

	for i := 0; i < 3; i++ {
		status, _ := performDownloadPath(t, "code=NOPE0002")
		require.Equal(t, 404, status)
	}

	assert.True(t, pickupBucketLocked(ctx, "NOPE0002"),
		"下载面：码确证不存在必须继续记账")
}
