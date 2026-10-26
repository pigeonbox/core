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

// newRetryTestConfigService 构造配置中心（返回 DB 句柄，便于制造/修复记录）。
func newRetryTestConfigService(t *testing.T) (*Service, *gorm.DB) {
	t.Helper()
	gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, gormDB.AutoMigrate(&model.SystemConfigRecord{}, &model.AdminOperationLog{}))
	sqlDB, err := gormDB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return NewService(gormDB), gormDB
}

// TestEnsureConfigLoaded_LoadFailureRetries 加载失败**不得**把配置钉死在默认值。
//
// 缺陷（2026-10-26 审计 P2）：ensureConfigLoaded 在 loadPersistedConfig **之前**
// 就置 configLoaded=true，且失败分支不复位 ⇒ 一次 DB 抖动 / 记录 JSON 损坏 ⇒
// 管理员配的持久化设置整体失效并回落默认值（download.require_login 默认关 ⇒
// 匿名可下载；upload.open_upload、匿名日配额等安全/配额语义被悄悄放开），
// 且本进程内永不重试（只有一条 Error 日志）。
//
// 断言必须能变红（负对照）：把 configLoaded=true 移回加载之前，本用例即失败
// （第二次 GetConfig 拿不到持久化值 / configLoaded 已被标记）。
func TestEnsureConfigLoaded_LoadFailureRetries(t *testing.T) {
	svc, gormDB := newRetryTestConfigService(t)
	ctx := context.Background()

	// 全局 conf 是进程级单例 ⇒ 本用例会改它（重试成功时热应用）⇒ 测完还原。
	oldGlobal := conf.GetGlobalConfig()
	conf.SetGlobalConfig(&conf.AppConfiguration{})
	t.Cleanup(func() { conf.SetGlobalConfig(oldGlobal) })

	// 持久化记录存在但 **JSON 损坏** ⇒ 首次加载失败
	require.NoError(t, gormDB.Create(&model.SystemConfigRecord{
		Data: "{ this is not valid json",
	}).Error)

	// 首次读：拿到默认值（服务不中断），但**不得**标记已加载
	got, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.Base)
	require.Equal(t, "PigeonBox", got.Base.Name, "加载失败时用默认值服务")
	require.False(t, svc.configLoaded, "加载失败不得标记已加载（否则永不重试）")

	// 修复记录（运维/下一次写入把 JSON 写好）
	require.NoError(t, gormDB.Model(&model.SystemConfigRecord{}).
		Where("id = ?", 1).
		Update("data", `{"base":{"name":"修复后的站点","description":"d","port":8888},`+
			`"download":{"require_login":true}}`).Error)

	// 第二次读：自动重试 ⇒ 拿到持久化值
	got, err = svc.GetConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, got.Base)
	require.Equal(t, "修复后的站点", got.Base.Name, "加载失败后必须自动重试")
	require.True(t, svc.configLoaded, "加载成功后标记已加载")

	// ③ 运行时闸门读的是**全局 conf** ⇒ 重试成功后必须热应用回去，
	// 否则「配置中心恢复了、闸门还钉在默认值」（失败期间 require_login 被放开）。
	require.NotNil(t, conf.GetGlobalConfig())
	require.True(t, conf.GetGlobalConfig().Download.RequireLogin,
		"重试成功后必须把持久化配置热应用回全局 conf（闸门读的是全局 conf）")
}
