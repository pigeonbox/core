package user

// 用户侧「已用空间」口径守卫（GetStats.TotalStorage）。
//
// 首页/个人中心显示的已用空间与上传时 CheckQuota 的判定必须同源，否则会出现
// 「显示还有空间、上传却被拒」或反过来的自相矛盾。CheckQuota 一直用
// file_codes 实时聚合（GetTotalSizeByUserID）；GetStats 此前返回
// users.total_storage 这个只增不减的漂移计数器 ⇒ 本次统一到聚合。

import (
	"context"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func newStatsTestService(t *testing.T) (*Service, *gorm.DB) {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.User{}, &model.UserAPIKey{}, &model.FileCode{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return NewService(g), g
}

func TestGetStats_TotalStorageUsesRealtimeAggregate(t *testing.T) {
	svc, g := newStatsTestService(t)
	ctx := context.Background()

	u := &model.User{
		Username:     "statsuser",
		Email:        "statsuser@test.local",
		Status:       "active",
		Role:         "user",
		TotalStorage: 5264909650, // 漂移计数器（线上实录的形态）
	}
	require.NoError(t, g.Create(u).Error)
	g.Create(&model.FileCode{Code: "ST000001", Size: 100, UserID: &u.ID})
	g.Create(&model.FileCode{Code: "ST000002", Size: 250, UserID: &u.ID})
	// 软删的分享不计入
	gone := &model.FileCode{Code: "ST000003", Size: 4096, UserID: &u.ID}
	g.Create(gone)
	require.NoError(t, g.Delete(gone).Error)

	stats, err := svc.GetStats(ctx, u.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 350, stats.TotalStorage,
		"已用空间必须按 file_codes 聚合（软删不计），不能返回计数器")
	assert.Equal(t, 2, stats.FileCount, "文件数同样只计未软删的分享")

	// 与配额判定同源：CheckQuota 用的就是这个数（无限额配置 ⇒ 直接放行）
	require.NoError(t, svc.CheckQuota(ctx, u.ID, 0))
}
