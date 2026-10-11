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

// newInjectTestDB 开一个隔离的 :memory: 库并迁移 FileCode 表。
// 注意：glebarez/sqlite 的 :memory: 每条连接是独立库，钉死单连接
// （同 filecode_governance_test.go 的 newGovernanceTestDB 处理方式）。
func newInjectTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return g
}

// TestInjectedRepositoryWorks 注入消费走通 Create + GetByCode，
// 证明 repository 全部读写落在注入实例（去全局化终态）。
func TestInjectedRepositoryWorks(t *testing.T) {
	g := newInjectTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	require.NoError(t, repo.Create(ctx, &model.FileCode{Code: "INJECT01", Status: model.StatusNormal}))

	got, err := repo.GetByCode(ctx, "INJECT01")
	require.NoError(t, err)
	assert.Equal(t, "INJECT01", got.Code)
	assert.Equal(t, model.StatusNormal, got.Status)
}

// TestInjectedRepositoriesAreIsolated 注入隔离性：两个独立 :memory: 实例
// 各自注入的 repository 互不可见对方数据（构造必填后不存在全局回退路径）。
func TestInjectedRepositoriesAreIsolated(t *testing.T) {
	g1, g2 := newInjectTestDB(t), newInjectTestDB(t)
	repo1, repo2 := NewFileCodeRepository(g1), NewFileCodeRepository(g2)
	ctx := context.Background()

	require.NoError(t, repo1.Create(ctx, &model.FileCode{Code: "ONLYIN01", Status: model.StatusNormal}))

	_, err := repo2.GetByCode(ctx, "ONLYIN01")
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound, "实例 2 不得读到实例 1 的数据")

	// 同实例注入的两个 repo 读写同一份库
	repo1b := NewFileCodeRepository(g1)
	got, err := repo1b.GetByCode(ctx, "ONLYIN01")
	require.NoError(t, err)
	assert.Equal(t, "ONLYIN01", got.Code)
}
