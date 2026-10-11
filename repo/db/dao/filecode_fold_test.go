package dao

import (
	"context"
	"errors"
	"testing"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 大小写折叠查询（UPPER 归一）：查询折叠开启时的 DB 兜底路径。
// 码表全 ASCII，sqlite UPPER 即够；MySQL ci collation 下精确匹配本就折叠，
// 折叠查询为幂等兜底。

func newFoldTestDB(t *testing.T) *FileCodeRepository {
	t.Helper()
	g := newGovernanceTestDB(t)
	return NewFileCodeRepository(g)
}

func TestGetByCodeFolded(t *testing.T) {
	repo := newFoldTestDB(t)
	ctx := context.Background()
	require.NoError(t, repo.Create(ctx, &model.FileCode{Code: "AbCd1234"}))

	t.Run("小写输入命中大写存储", func(t *testing.T) {
		fc, err := repo.GetByCodeFolded(ctx, "abcd1234")
		require.NoError(t, err)
		assert.Equal(t, "AbCd1234", fc.Code)
	})
	t.Run("精确与折叠一致时同一行", func(t *testing.T) {
		exact, err := repo.GetByCode(ctx, "AbCd1234")
		require.NoError(t, err)
		folded, err := repo.GetByCodeFolded(ctx, "AbCd1234")
		require.NoError(t, err)
		assert.Equal(t, exact.ID, folded.ID)
	})
	t.Run("无匹配返回 RecordNotFound", func(t *testing.T) {
		_, err := repo.GetByCodeFolded(ctx, "zzzz9999")
		assert.True(t, errors.Is(err, gorm.ErrRecordNotFound))
	})
	t.Run("软删行不参与折叠命中", func(t *testing.T) {
		soft := &model.FileCode{Code: "SoftDel1"}
		require.NoError(t, repo.Create(ctx, soft))
		require.NoError(t, repo.Delete(ctx, soft.ID))
		_, err := repo.GetByCodeFolded(ctx, "softdel1")
		assert.True(t, errors.Is(err, gorm.ErrRecordNotFound))
	})
}

func TestCheckCodeExistsFolded(t *testing.T) {
	repo := newFoldTestDB(t)
	ctx := context.Background()
	fc := &model.FileCode{Code: "MyCode"}
	require.NoError(t, repo.Create(ctx, fc))

	t.Run("仅大小写不同判占用", func(t *testing.T) {
		exists, err := repo.CheckCodeExistsFolded(ctx, "mycode", 0)
		require.NoError(t, err)
		assert.True(t, exists)
	})
	t.Run("排除自身后不误报", func(t *testing.T) {
		exists, err := repo.CheckCodeExistsFolded(ctx, "MYCODE", fc.ID)
		require.NoError(t, err)
		assert.False(t, exists)
	})
	t.Run("无关码不占用", func(t *testing.T) {
		exists, err := repo.CheckCodeExistsFolded(ctx, "other1", 0)
		require.NoError(t, err)
		assert.False(t, exists)
	})
}
