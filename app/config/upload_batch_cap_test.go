package config

// 「单次发送**总量**上限」（user.user_upload_size）的生效口径断言（2026-10-30）。
//
// 这个值与 upload.max_file_size（**单文件**上限）是两个不同的闸，2026-10-30
// 之前前端只拿到后者 ⇒ UI 显示「单文件最大 4GB」而登录用户被总量闸卡在
// 50MB（报「上传总大小超过限制（上限 52428800 字节）」）。
//
// 三条断言：
//  1. 无持久化记录 ⇒ 回退 yaml 的 user 段（不是 0，也不是别的默认值）；
//  2. 管理台持久化值优先（DB 段生效）；
//  3. 本值是**站点级默认**——per-user 覆盖（users.max_upload_size）不参与，
//     登录用户的实际上限以服务端 413 为准（公开面只放站点默认）。

import (
	"context"
	"testing"

	"github.com/pigeonbox/core/conf"
	"github.com/stretchr/testify/require"
)

func TestEffectiveUploadBatchCap(t *testing.T) {
	ctx := context.Background()

	t.Run("无持久化记录 ⇒ 回退 yaml 的 user 段", func(t *testing.T) {
		old := conf.GetGlobalConfig()
		conf.SetGlobalConfig(&conf.AppConfiguration{
			User: conf.UserConfig{UserUploadSize: 12345},
		})
		t.Cleanup(func() { conf.SetGlobalConfig(old) })

		svc := newTestConfigService(t)
		require.Equal(t, int64(12345), svc.EffectiveUploadBatchCap(ctx),
			"yaml 兜底必须取全局配置的 user.user_upload_size")
	})

	t.Run("管理台持久化值优先", func(t *testing.T) {
		svc := newTestConfigService(t)
		require.NoError(t, svc.UpdateUserSettings(ctx, UserSettings{
			UserUploadSize:     268435456, // 256MB
			UserStorageQuota:   1073741824,
			SessionExpiryHours: 168,
		}))
		require.Equal(t, int64(268435456), svc.EffectiveUploadBatchCap(ctx),
			"管理台改过的总量上限必须生效（与 GetUploadSizeCap 的系统默认层同源）")
	})

	t.Run("0 = 不限", func(t *testing.T) {
		svc := newTestConfigService(t)
		require.NoError(t, svc.UpdateUserSettings(ctx, UserSettings{
			UserUploadSize:     0,
			UserStorageQuota:   1073741824,
			SessionExpiryHours: 168,
		}))
		require.Equal(t, int64(0), svc.EffectiveUploadBatchCap(ctx),
			"0 = 不限（2026-10-30 起出厂默认）")
	})
}
