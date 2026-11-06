package admin

// 管理台用户列表的「存储占用」口径守卫（GetUsers / ListUsersFiltered）。
//
// ## 为什么要有
//
// 线上实录（4.9.0，10.10.10.3）：管理台用户页显示的已用空间与磁盘/分享表
// 完全对不上——某用户计数器 4.9GB 而 file_codes 里 0 行，另一用户计数器
// 8MB 实际 0。根因是列表直接读 `users.total_storage`（只增不减的漂移计数器），
// 而配额判定 CheckQuota 与用户侧 GetStats 都用 file_codes 实时聚合
// ⇒ 列表说超配额、上传却放行。
//
// ## 守卫点
//
//  1. 列表数字 = file_codes 实时聚合（软删不计），**不是**计数器；
//  2. 没有任何分享的用户显示 0（不是历史累计值）；
//  3. 不碰 quota_limit 等其它字段（只修口径，不改语义）。

import (
	"context"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func newUsersStorageTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.User{}, &model.FileCode{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1) // glebarez/sqlite :memory: 每连接独立库
	return g
}

func TestGetUsers_UsesRealtimeStorageAggregate(t *testing.T) {
	g := newUsersStorageTestDB(t)
	ctx := context.Background()

	// 计数器与真实占用**故意背离**：这正是线上形态
	users := []*model.User{
		{Username: "admin", Email: "admin@test.local", Status: "active", Role: "admin", TotalStorage: 8 * 1024 * 1024},
		{Username: "kylin", Email: "kylin@test.local", Status: "active", Role: "user", TotalStorage: 5264909650},
		{Username: "withshares", Email: "ws@test.local", Status: "active", Role: "user", TotalStorage: 999},
	}
	for _, u := range users {
		require.NoError(t, g.Create(u).Error)
	}

	g.Create(&model.FileCode{Code: "WS000001", Size: 10, UserID: &users[2].ID})
	g.Create(&model.FileCode{Code: "WS000002", Size: 20, UserID: &users[2].ID})

	svc := NewService(g)
	resps, total, err := svc.GetUsers(ctx, 1, 20)
	require.NoError(t, err)
	require.EqualValues(t, 3, total)

	byName := make(map[string]*model.UserResp, len(resps))
	for _, r := range resps {
		byName[r.Username] = r
	}
	require.Contains(t, byName, "admin")
	require.Contains(t, byName, "kylin")
	require.Contains(t, byName, "withshares")

	assert.EqualValues(t, 0, byName["admin"].TotalStorage,
		"无分享记录 ⇒ 占用 0，不是计数器里的 8MB")
	assert.EqualValues(t, 0, byName["kylin"].TotalStorage,
		"无分享记录 ⇒ 占用 0，不是计数器里的 4.9GB")
	assert.EqualValues(t, 30, byName["withshares"].TotalStorage,
		"有分享 ⇒ 按 file_codes 求和，不是计数器里的 999")
}

func TestListUsersFiltered_UsesRealtimeStorageAggregate(t *testing.T) {
	g := newUsersStorageTestDB(t)
	ctx := context.Background()

	u := &model.User{Username: "filtered", Email: "filtered@test.local", Status: "active", Role: "user", TotalStorage: 777}
	require.NoError(t, g.Create(u).Error)
	g.Create(&model.FileCode{Code: "FL000001", Size: 64, UserID: &u.ID})

	svc := NewService(g)
	resps, total, err := svc.ListUsersFiltered(ctx, UserListFilter{Page: 1, PageSize: 20})
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Len(t, resps, 1)
	assert.EqualValues(t, 64, resps[0].TotalStorage,
		"筛选列表与全量列表必须同口径（同一页面两个数字会互相矛盾）")
}
