package dao

import (
	"context"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func newExpireTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}))
	// glebarez/sqlite 的 :memory: 每条连接是独立库，多连接会拿到无表空库；
	// 钉死单连接消除该 flake（异步 goroutine 与主流程并发取连接时必现）。
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return g
}

func TestDecrementExpiredCount_Limited(t *testing.T) {
	g := newExpireTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()
	require.NoError(t, repo.Create(ctx, &model.FileCode{Code: "C1", ExpiredCount: 2}))

	ok, err := repo.DecrementExpiredCount(ctx, "C1")
	require.NoError(t, err)
	assert.True(t, ok)

	fc, err := repo.GetByCode(ctx, "C1")
	require.NoError(t, err)
	assert.Equal(t, 1, fc.ExpiredCount)
	assert.Equal(t, 1, fc.UsedCount)
}

func TestDecrementExpiredCount_Exhausted(t *testing.T) {
	g := newExpireTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()
	require.NoError(t, repo.Create(ctx, &model.FileCode{Code: "C2", ExpiredCount: 1}))

	ok1, err := repo.DecrementExpiredCount(ctx, "C2")
	require.NoError(t, err)
	assert.True(t, ok1)
	ok2, err := repo.DecrementExpiredCount(ctx, "C2") // 已耗尽
	require.NoError(t, err)
	assert.False(t, ok2)

	fc, err := repo.GetByCode(ctx, "C2")
	require.NoError(t, err)
	assert.Equal(t, 0, fc.ExpiredCount)
	assert.Equal(t, 1, fc.UsedCount) // 第二次没扣成功，used_count 不增
}

func TestDecrementExpiredCount_Unlimited(t *testing.T) {
	g := newExpireTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()
	require.NoError(t, repo.Create(ctx, &model.FileCode{Code: "C3", ExpiredCount: -1}))

	for i := 0; i < 5; i++ {
		ok, err := repo.DecrementExpiredCount(ctx, "C3")
		require.NoError(t, err)
		assert.True(t, ok, "无限次数始终成功")
	}
	fc, err := repo.GetByCode(ctx, "C3")
	require.NoError(t, err)
	assert.Equal(t, -1, fc.ExpiredCount) // 无限不变
	assert.Equal(t, 5, fc.UsedCount)
}

func TestDecrementExpiredCount_NotFound(t *testing.T) {
	g := newExpireTestDB(t)
	repo := NewFileCodeRepository(g)
	ok, err := repo.DecrementExpiredCount(context.Background(), "NOEXIST")
	require.NoError(t, err)
	assert.False(t, ok) // 不存在 → 0 行受影响 → ok=false
}
