package config

import (
	"context"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/repo/db/model"
)

func newTestConfigService(t *testing.T) *Service {
	t.Helper()
	gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, gormDB.AutoMigrate(&model.SystemConfigRecord{}, &model.AdminOperationLog{}))
	// glebarez/sqlite :memory: 每连接独立库，钉单连接防 flake（同 share 域测试）
	sqlDB, err := gormDB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return NewService(gormDB)
}

// TestUpdateConfigPartialPreservesValueSections 局部保存（只携带部分段）不得把
// base/storage/transfer 清零。2026-10-12 回归：此三段曾为匿名值类型、不在 nil 保留
// 清单内，管理端任一分区保存（如只带 oidc）都会把站点名/端口/传输默认值/存储段写坏。
func TestUpdateConfigPartialPreservesValueSections(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()

	// 只带 oidc 段（等价管理端「用户与登录」分区的提交形态）
	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		OIDC: &conf.OIDCConfig{Enabled: false},
	}))

	got, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, got.Base)
	require.Equal(t, "PigeonBox", got.Base.Name)
	require.Equal(t, 8888, got.Base.Port)
	require.NotNil(t, got.Storage)
	require.Equal(t, "local", got.Storage.Type)
	require.NotNil(t, got.Transfer)
	require.Equal(t, 100, got.Transfer.MaxCount)
	require.Equal(t, 7, got.Transfer.ExpireDefault)
	// 提交的段正常生效
	require.NotNil(t, got.OIDC)

	// 显式提交仍整体覆盖；未提交段继续保留
	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		Base: &BaseConfig{Name: "自定义站点", Description: "desc", Port: 9000},
	}))
	got, err = svc.GetConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, "自定义站点", got.Base.Name)
	require.Equal(t, 9000, got.Base.Port)
	require.Equal(t, 100, got.Transfer.MaxCount)
}
