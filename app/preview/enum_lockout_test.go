// 预览入口的枚举锁负向（core#183 残差 S03a/#175：四入口统一锁的逐点钉之一）。
//
// preview 的预检在 authorizePickup 里、**解析之前**（app/preview/service.go
// 的 PickupLockedRemain）⇒ 与 metadata 入口同形：记满后连不存在的码也必须
// 进锁定态（HTTP 适配层映射 429）。
//
// 缺口：本域此前没有任何 LockedError 用例 ⇒ 撤掉该预检，预览面的枚举防护
// 静默塌成只剩域内 CheckPickup（它在解析成功之后才跑，不存在的码永远到不了）
// ⇒ 枚举本面归零，而测试全绿。
//
// 断言必须能变红（已实测）：删掉 authorizePickup 里的 PickupLockedRemain 预检
// ⇒ 第 11 次落回 ErrShareNotFound（不存在的码到不了 CheckPickup）⇒ 本用例转红。
package preview

import (
	"context"
	"fmt"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/repo/db/model"
)

// withPreviewLockout 设失败锁定阈值并重建全局锁定器（内存计数随重建清空），
// 保证用例间计数不串扰——与 gen/handler/share 的 withPickupLockout 同规格。
func withPreviewLockout(t *testing.T, maxAttempts int) {
	t.Helper()
	orig := conf.GetGlobalConfig()
	base := orig
	if base == nil {
		base = &conf.AppConfiguration{}
	}
	old := base.Security.Lockout
	base.Security.Lockout = conf.LockoutConfig{
		Enabled: true, MaxAttempts: maxAttempts, WindowSeconds: 300, LockSeconds: 600,
	}
	conf.SetGlobalConfig(base)
	middleware.InitDefaultLockout(nil)
	t.Cleanup(func() {
		base.Security.Lockout = old
		conf.SetGlobalConfig(orig) // 原样恢复（含 nil），不给后续用例留残留配置
		middleware.InitDefaultLockout(nil)
	})
}

// TestPreview_EnumLockoutAfterTenMisses 预览入口：10 个互不相同的不存在码
// 记满按 IP 的枚举桶 ⇒ 第 11 次必须进 LockedError。
func TestPreview_EnumLockoutAfterTenMisses(t *testing.T) {
	gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := gormDB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, gormDB.AutoMigrate(&model.FileCode{}, &model.FilePreview{}))

	withPreviewLockout(t, 10)
	svc := NewService(gormDB, nil)
	ctx := context.Background()
	ip := "203.0.113.42" // 显式非空 IP：枚举维度按 IP 累计，注入值保证用例隔离

	// 前 10 次：互不相同的不存在码 ⇒ ErrShareNotFound（入枚举桶），不得提前锁定
	for i := 0; i < 10; i++ {
		code := fmt.Sprintf("NOPV%04d", i)
		_, err := svc.GetOrCreate(ctx, GetReq{Code: code, ClientIP: ip})
		require.ErrorIs(t, err, ErrShareNotFound, "第 %d 次不存在码应为 not-found（记账中）: %v", i+1, err)
	}

	// 第 11 次：按 IP 的枚举维度已锁 ⇒ 预检在解析前命中 ⇒ LockedError
	_, err = svc.GetOrCreate(ctx, GetReq{Code: "NOPV9999", ClientIP: ip})
	var locked *LockedError
	require.ErrorAs(t, err, &locked, "枚举锁定后 preview 入口必须返回 LockedError（HTTP 层映射 429），实得 %v", err)
	require.Greater(t, locked.Remain, 0, "锁定态必须携带剩余秒数（供文案）")
}
