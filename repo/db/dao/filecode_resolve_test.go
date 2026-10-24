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

// 按码解析是**全站唯一真相源**（分享域/预览域/匿名域/MCP 四家共用）⇒ 它的
// 优先级与归一口径一旦漂移，四个面会各说各话。
//
// 2026-10-24：解析器从 pkg/codex 下沉到 dao 层（原实现 import repo/db/model ⇒
// 违反架构守卫规则 1「pkg 不得依赖上层」）。断言原样保留。
func newResolveTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return g
}

func TestResolveByCode_Priority(t *testing.T) {
	g := newResolveTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	share := &model.FileCode{Code: "AbCd1234"}
	pickup := &model.FileCode{Code: "XyZ0987"}
	pickupCode := "WS9WHP"
	pickup.PickupCode = &pickupCode
	require.NoError(t, repo.Create(ctx, share))
	require.NoError(t, repo.Create(ctx, pickup))

	t.Run("8 位分享码命中 code 列", func(t *testing.T) {
		fc, err := repo.ResolveByCode(ctx, "AbCd1234", true)
		require.NoError(t, err)
		assert.Equal(t, "AbCd1234", fc.Code)
	})

	t.Run("大小写折叠兜底", func(t *testing.T) {
		fc, err := repo.ResolveByCode(ctx, "abcd1234", true)
		require.NoError(t, err)
		assert.Equal(t, "AbCd1234", fc.Code)
	})

	t.Run("折叠关闭时不兜底", func(t *testing.T) {
		_, err := repo.ResolveByCode(ctx, "abcd1234", false)
		assert.Error(t, err)
	})

	t.Run("6 位取件码命中取件码列（并归一为大写）", func(t *testing.T) {
		fc, err := repo.ResolveByCode(ctx, "ws9whp", true)
		require.NoError(t, err)
		assert.Equal(t, "XyZ0987", fc.Code)
	})

	t.Run("首尾空白容忍（分享码与取件码同口径）", func(t *testing.T) {
		fc, err := repo.ResolveByCode(ctx, "  AbCd1234  ", true)
		require.NoError(t, err)
		assert.Equal(t, "AbCd1234", fc.Code)
	})

	t.Run("全查不到 ⇒ 报错", func(t *testing.T) {
		_, err := repo.ResolveByCode(ctx, "ZZZZZZ", true)
		assert.Error(t, err)
	})
}

// 占位判定（未完成上传）必须只有一处 —— 分散在多域会各自漂移。
func TestIsNotReady(t *testing.T) {
	// 占位：既无文件路径、也无文本
	assert.True(t, model.IsNotReady(&model.FileCode{}))
	// 文本分享：Text 非空 ⇒ 不是占位
	assert.False(t, model.IsNotReady(&model.FileCode{Text: "hello"}))
	// 文件分享：有文件路径 ⇒ 不是占位
	assert.False(t, model.IsNotReady(&model.FileCode{FilePath: "uploads/x/a.bin"}))
	// nil ⇒ 不判为占位（避免把"没查到"误报成"未上传"）
	assert.False(t, model.IsNotReady(nil))
}
