package dao

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/oekit/gormx"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// newSystemConfigTestDB 开一个隔离的 :memory: 库并迁移 system_configs 表。
// glebarez/sqlite 的 :memory: 每条连接是独立库，必须钉死单连接
// （同 inject_test.go 的 newInjectTestDB 处理方式），并发事务由连接池串行化。
func newSystemConfigTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.SystemConfigRecord{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return g
}

// TestSystemConfigSaveConcurrentConverges 并发 Save 收敛单行：N 路并发携带
// 不同 data 的 Save 不得双行、不得泄漏错误，终态必为其中一份完整 data。
func TestSystemConfigSaveConcurrentConverges(t *testing.T) {
	g := newSystemConfigTestDB(t)
	repo := NewSystemConfigRepository(g)
	ctx := context.Background()

	const n = 20
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = repo.Save(ctx, &model.SystemConfigRecord{Data: fmt.Sprintf(`{"v":%d}`, i)})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "goroutine %d Save 报错", i)
	}

	var cnt int64
	require.NoError(t, g.Model(&model.SystemConfigRecord{}).Count(&cnt).Error)
	assert.Equal(t, int64(1), cnt, "并发 Save 后必须收敛到单行")

	var rec model.SystemConfigRecord
	require.NoError(t, g.First(&rec).Error)
	assert.Regexp(t, `^\{"v":\d+\}$`, rec.Data, "终态 data 必为某一并发方完整一份")
}

// TestSystemConfigSaveUpdatePreservesIdentity 已有行时 Save 走更新路径：
// 不新增行，保留原 ID/CreatedAt，data 全量替换。
func TestSystemConfigSaveUpdatePreservesIdentity(t *testing.T) {
	g := newSystemConfigTestDB(t)
	repo := NewSystemConfigRepository(g)
	ctx := context.Background()

	require.NoError(t, repo.Save(ctx, &model.SystemConfigRecord{Data: `{"v":1}`}))
	saved, err := repo.Get(ctx)
	require.NoError(t, err)
	require.NotNil(t, saved)

	require.NoError(t, repo.Save(ctx, &model.SystemConfigRecord{Data: `{"v":2}`}))
	got, err := repo.Get(ctx)
	require.NoError(t, err)
	assert.Equal(t, saved.ID, got.ID, "更新路径不得换行")
	assert.Equal(t, saved.CreatedAt.Unix(), got.CreatedAt.Unix(), "CreatedAt 须保留")
	assert.Equal(t, `{"v":2}`, got.Data)

	var cnt int64
	require.NoError(t, g.Model(&model.SystemConfigRecord{}).Count(&cnt).Error)
	assert.Equal(t, int64(1), cnt)
}

// TestSystemConfigSaveUniqueRetryConverges 撞键重试收敛（确定性复现并发首写）：
// 本事务锁读已确认无行、Create 执行前的瞬间，竞争连接插入并提交唯一行——
// 本事务 Create 撞真实唯一约束（或单写者快照过期 busy），Save 须整体重试一次、
// 新事务看到胜出行并转更新路径收敛，不泄漏错误、不产生第二行。
// 用文件库+双连接（:memory: 每连接独立库无法复现跨连接竞争，故不用钉单连接）。
func TestSystemConfigSaveUniqueRetryConverges(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "syscfg.db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	g, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.SystemConfigRecord{}))
	rival, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)

	repo := NewSystemConfigRepository(g)
	ctx := context.Background()

	var primed bool
	_ = g.Callback().Create().Before("gorm:create").Register("rival_write", func(tx *gorm.DB) {
		if primed {
			return
		}
		primed = true
		// 竞争首写：另一连接落库提交唯一行；同时把本事务的插入目标改为
		// 与胜出行冲突的主键，令本次 Create 实际撞键。
		require.NoError(t, rival.Exec(
			`INSERT INTO system_configs (id, created_at, updated_at, data)
			 VALUES (1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, '{"v":1}')`).Error)
		if rec, ok := tx.Statement.Dest.(*model.SystemConfigRecord); ok {
			rec.ID = 1
		}
	})

	require.NoError(t, repo.Save(ctx, &model.SystemConfigRecord{Data: `{"v":2}`}),
		"撞键后应整体重试一次并收敛，不得外泄错误")

	got, err := repo.Get(ctx)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, uint(1), got.ID, "重试应看到胜出行并走更新路径")
	assert.Equal(t, `{"v":2}`, got.Data, "重试须转更新路径收敛为第二份数据")

	var cnt int64
	require.NoError(t, g.Model(&model.SystemConfigRecord{}).Count(&cnt).Error)
	assert.Equal(t, int64(1), cnt)
}

// TestIsUniqueViolation 撞唯一键判据表驱动：gorm 映射分支 + 三家驱动裸 SQL
// 文案（mysql/pg 无需真库，直接喂驱动实际报错文案）+ 反例锁窄匹配。
//
// 判据本体已**收编上游 oekit/gormx**（core 抬 gorm 地板到 v1.31.2 后直接依赖；
// 见 repo/db/dao/unique_violation.go 的演进注释）⇒ 本用例改为**锁上游行为**：
// 上游的窄判据一旦改动（措辞/宽窄），这里即红 ⇒ 避免"上游改了、本仓静默失配"。
func TestIsUniqueViolation(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"gorm 映射（TranslateError 开启时）", fmt.Errorf("wrapped: %w", gorm.ErrDuplicatedKey), true},
		{"sqlite 裸 SQL", errors.New(
			"INSERT INTO system_configs ...: UNIQUE constraint failed: system_configs.id"), true},
		{"mysql 1062 裸文案", errors.New(
			"Error 1062: Duplicate entry '1' for key 'system_configs.PRIMARY'"), true},
		{"pg 23505 裸文案", errors.New(
			`ERROR: duplicate key value violates unique constraint "system_configs_pkey" (SQLSTATE 23505)`), true},
		{"mysql 小写不命中（精确大小写）", errors.New(
			"duplicate entry '1' for key 'PRIMARY'"), false},
		{"裸 constraint failed 不命中（窄判据）", errors.New("some constraint failed"), false},
		{"pg 23505 单独出现（无文案）", errors.New("ERROR: (23505)"), true},
		{"io 错误不命中", errors.New("disk I/O error"), false},
		{"ctx 超时不命中", context.DeadlineExceeded, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, gormx.IsUniqueViolation(tc.err))
		})
	}
}

// TestSystemConfigSaveNotInitialized 全局 DB 未初始化 fail-fast。
func TestSystemConfigSaveNotInitialized(t *testing.T) {
	repo := NewSystemConfigRepository(nil)
	err := repo.Save(context.Background(), &model.SystemConfigRecord{Data: "{}"})
	assert.ErrorIs(t, err, ErrDBNotInitialized)
}
