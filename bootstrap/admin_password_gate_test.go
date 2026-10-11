package bootstrap

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 生产模式拒绝默认 admin 口令门禁（2026-10-08 审计：admin123 此前仅 Warn，
// 与 JWT secret 的 fail-fast 不对称）。守卫语义：
//   - 生产 + 无 PB_ADMIN_PASSWORD + 库中无 admin → ErrInsecureDefaultAdmin，不建号
//   - 生产 + 有 PB_ADMIN_PASSWORD → 正常建号
//   - 非生产 + 无 env → 维持旧行为（admin123 + Warn）
//   - 库中已有 admin → 恒放行（存量部署升级不受 env 缺失影响）
func TestCreateDefaultAdmin_ProductionGate(t *testing.T) {
	newAdminDB := func(t *testing.T) *gorm.DB {
		t.Helper()
		gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
		require.NoError(t, err)
		require.NoError(t, gormDB.AutoMigrate(&model.User{}))
		// glebarez/sqlite :memory: 多连接各为独立库，钉死单连接防 flake
		sqlDB, err := gormDB.DB()
		require.NoError(t, err)
		sqlDB.SetMaxOpenConns(1)
		return gormDB
	}
	productionCfg := &conf.AppConfiguration{}
	productionCfg.App.Production = true
	adminCount := func(t *testing.T, gormDB *gorm.DB) int64 {
		t.Helper()
		var n int64
		gormDB.Model(&model.User{}).Where("role = ?", "admin").Count(&n)
		return n
	}

	t.Run("production_without_password_refused", func(t *testing.T) {
		t.Setenv("PB_ADMIN_PASSWORD", "")
		gormDB := newAdminDB(t)
		err := CreateDefaultAdmin(gormDB, productionCfg)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrInsecureDefaultAdmin), "必须可经 errors.Is 识别为哨兵错误")
		assert.Zero(t, adminCount(t, gormDB), "拒绝时不得落库默认口令账号")
	})

	t.Run("production_with_password_ok", func(t *testing.T) {
		t.Setenv("PB_ADMIN_PASSWORD", "s3cure-Pr0d-Passw0rd!")
		gormDB := newAdminDB(t)
		require.NoError(t, CreateDefaultAdmin(gormDB, productionCfg))
		assert.EqualValues(t, 1, adminCount(t, gormDB))
	})

	t.Run("dev_without_password_legacy_behavior", func(t *testing.T) {
		t.Setenv("PB_ADMIN_PASSWORD", "")
		gormDB := newAdminDB(t)
		require.NoError(t, CreateDefaultAdmin(gormDB, nil), "cfg 为 nil 视为非生产，保持旧行为")
		assert.EqualValues(t, 1, adminCount(t, gormDB))
	})

	t.Run("existing_admin_bypasses_env_check", func(t *testing.T) {
		t.Setenv("PB_ADMIN_PASSWORD", "")
		gormDB := newAdminDB(t)
		require.NoError(t, CreateDefaultAdmin(gormDB, nil)) // 先建一个 admin
		require.NoError(t, CreateDefaultAdmin(gormDB, productionCfg), "库中已有 admin 时生产模式也不得拒绝（存量部署升级路径）")
		assert.EqualValues(t, 1, adminCount(t, gormDB))
	})
}
