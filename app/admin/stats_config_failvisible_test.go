// core#137 判据：app/admin GetStats/GetConfig 在坏库下必须**显式失败**，
// 不再静默回退零值统计/默认配置（故障期 200 全 0 无错误态 = 假数据，
// 与 core#102「失败必须可见」相悖）。
//
// 负对照（已实跑）：
//   - GetStats 回退 `if err == nil { 赋值 }` 吞错 ⇒ 「坏库必须报错」判据转红；
//   - GetConfig 门面去掉 LoadError 检查 ⇒ 「坏库/坏记录必须报错」判据转红。
package admin

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/repo/db/model"
)

// newBrokenTestDB 未建表的内存库 ⇒ 一切查询稳定失败（坏库驱动）。
func newBrokenTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return g
}

// newStatsTestDB 建好 GetStats 所需全表的健康库。
func newStatsTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	g := newAdminTestDB(t)
	require.NoError(t, g.AutoMigrate(&model.User{}))
	return g
}

func TestGetStats_BrokenDBFailsVisible(t *testing.T) {
	ctx := context.Background()

	t.Run("坏库 ⇒ 显式错误（修前 200 全 0）", func(t *testing.T) {
		svc := NewService(newBrokenTestDB(t))
		stats, err := svc.GetStats(ctx)
		assert.Error(t, err, "坏库下 GetStats 必须报错，不得静默回退零值统计")
		assert.Nil(t, stats, "失败不得给半成品统计")
	})

	t.Run("健康库 ⇒ 真实数值（成功路径零变化）", func(t *testing.T) {
		g := newStatsTestDB(t)
		svc := NewService(g)
		exp := time.Now().Add(time.Hour)
		require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
			Code: "STATS001", UUIDFileName: "a.bin", Size: 7, ExpiredAt: &exp, ExpiredCount: -1,
		}))
		require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
			Code: "STATS002", UUIDFileName: "b.bin", Size: 3, ExpiredCount: -1, // 永久
		}))

		stats, err := svc.GetStats(ctx)
		require.NoError(t, err)
		require.NotNil(t, stats)
		assert.Equal(t, int64(2), stats.TotalFiles)
		assert.Equal(t, int64(10), stats.TotalSize)
		assert.Equal(t, int64(1), stats.ForeverFiles, "永久口径：expired_at NULL 计入 forever_files")
	})
}

func TestGetConfig_BrokenDBFailsVisible(t *testing.T) {
	ctx := context.Background()

	t.Run("坏库 ⇒ 显式错误（修前 200 默认配置）", func(t *testing.T) {
		cfg, err := NewService(newBrokenTestDB(t)).GetConfig(ctx)
		assert.Error(t, err, "坏库下管理面 GetConfig 必须报错，不得把默认值当配置展示")
		assert.Nil(t, cfg)
	})

	t.Run("记录损坏 ⇒ 显式错误", func(t *testing.T) {
		g := newAdminTestDB(t)
		require.NoError(t, g.Create(&model.SystemConfigRecord{Data: "{corrupted"}).Error)
		cfg, err := NewService(g).GetConfig(ctx)
		assert.Error(t, err, "记录损坏必须报错，不得静默回退默认配置")
		assert.Nil(t, cfg)
	})

	t.Run("健康库无记录 ⇒ 默认配置（既有降级口径不动）", func(t *testing.T) {
		cfg, err := NewService(newAdminTestDB(t)).GetConfig(ctx)
		require.NoError(t, err, "「无记录」是合法状态，默认配置照常返回")
		require.NotNil(t, cfg)
		assert.Equal(t, "PigeonBox", cfg.Base.Name)
	})
}
