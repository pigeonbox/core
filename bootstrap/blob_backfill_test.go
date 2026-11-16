package bootstrap

import (
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/repo/db/model"
)

// 启动期账本回填补线（core#172 第二片）：RunDataMigrations 必须触发
// 存量回填，且**幂等**（重复启动不重复计数——负对照核心）。

func newBlobBackfillTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(
		&model.FileCode{}, &model.FileCodeFile{},
		&model.Blob{}, &model.FileReference{},
	))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return g
}

func TestRunDataMigrations_BackfillsBlobRefs(t *testing.T) {
	g := newBlobBackfillTestDB(t)

	// 老数据形状：有路径、账本两表为空
	require.NoError(t, g.Create(&model.FileCode{
		Code: "BFMIG01", FilePath: "uploads/bf/a.bin", Text: "a.bin",
		Size: 5, ExpiredCount: -1,
	}).Error)

	RunDataMigrations(g)

	var blobs, refs int64
	require.NoError(t, g.Model(&model.Blob{}).Count(&blobs).Error)
	require.NoError(t, g.Model(&model.FileReference{}).Count(&refs).Error)
	assert.Equal(t, int64(1), blobs, "启动迁移必须回填 blob")
	assert.Equal(t, int64(1), refs, "启动迁移必须回填 ref")

	// 幂等负对照：再跑一轮不产生第二行（中断再跑/重复启动不重复计数）
	RunDataMigrations(g)
	require.NoError(t, g.Model(&model.Blob{}).Count(&blobs).Error)
	require.NoError(t, g.Model(&model.FileReference{}).Count(&refs).Error)
	assert.Equal(t, int64(1), blobs, "重复启动 blob 行数不得增长")
	assert.Equal(t, int64(1), refs, "重复启动 ref 行数不得增长")
}

// 负对照：账本表未迁移（HasTable=false）⇒ 回填静默跳过，不报错不阻断启动。
func TestRunDataMigrations_BlobTablesMissing_SkipsBackfill(t *testing.T) {
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}, &model.FileCodeFile{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	require.NoError(t, g.Create(&model.FileCode{
		Code: "BFMIG02", FilePath: "uploads/bf/b.bin", Text: "b.bin",
		Size: 5, ExpiredCount: -1,
	}).Error)

	require.NotPanics(t, func() { RunDataMigrations(g) }, "缺账本表不得阻断启动迁移")

	var child model.FileCodeFile
	require.NoError(t, g.First(&child).Error, "既有迁移步骤照常执行")
}
