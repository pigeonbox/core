package config

import (
	"testing"

	"github.com/pigeonbox/core/conf"
	"github.com/stretchr/testify/require"
)

// TestValidateUserSettings 校验（自 app/admin/service_test.go 随域独立迁入）
func TestValidateUserSettings(t *testing.T) {
	require.Error(t, validateUserSettings(&UserSettings{UserUploadSize: -1}))
	require.Error(t, validateUserSettings(&UserSettings{SessionExpiryHours: -5}))
	require.Error(t, validateUserSettings(&UserSettings{SessionExpiryHours: 100000}))
	require.NoError(t, validateUserSettings(&UserSettings{SessionExpiryHours: 168}))
}

// TestValidateAllowedExpireStyles 钉住「管理台写过期样式白名单时，非法值即报错」。
//
// 2026-10-22：管理台此前没有这个字段的编辑器（后端接受、界面不暴露）⇒ 管理员
// 改不动；更糟的是写入非法样式后 bootstrap 的 effectiveExpireStyles 会**静默
// 过滤掉** ⇒ 「保存成功但设置没效果」且无任何提示。现在保存即报错并列出合法值。
func TestValidateAllowedExpireStyles(t *testing.T) {
	// 空 = 全部允许（合法语义）
	require.NoError(t, validateSystemConfig(&SystemConfig{
		UploadEx: &conf.UploadConfig{AllowedExpireStyles: nil},
	}))
	// 合法集合（含 count）通过
	require.NoError(t, validateSystemConfig(&SystemConfig{
		UploadEx: &conf.UploadConfig{AllowedExpireStyles: []string{"day", "count"}},
	}))
	// 非法样式报错（且错误信息里带合法值，便于管理员自我纠正）
	err := validateSystemConfig(&SystemConfig{
		UploadEx: &conf.UploadConfig{AllowedExpireStyles: []string{"day", "weeks"}},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "weeks")
	require.Contains(t, err.Error(), "count")
}
