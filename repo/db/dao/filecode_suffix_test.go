package dao

import (
	"context"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/repo/db/model"
)

func newSuffixStatsDB(t *testing.T) *gorm.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return g
}

// TestTopSuffixes_DerivesFromLiveColumns 回归（2026-10-26 P0）：
// 管理台「文件类型分布」曾直接查**已退役的 suffix 列** ⇒ ①该列本就恒空（widget
// 早就是死的）②退役后变成「SQL 报错被上层 `_` 吞掉」⇒ 恒空**且零日志**。
// ⇒ 现在从现役列（text 原始名 / file_path 末段）现算扩展名。
func TestTopSuffixes_DerivesFromLiveColumns(t *testing.T) {
	g := newSuffixStatsDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	// 有原始名（直传/分片通道）
	require.NoError(t, g.Create(&model.FileCode{Code: "A1", FilePath: "uploads/2026/10/26/a.pdf", Text: "报告.pdf"}).Error)
	require.NoError(t, g.Create(&model.FileCode{Code: "A2", FilePath: "uploads/2026/10/26/b.pdf", Text: "合同.pdf"}).Error)
	// 多点扩展名取**最后**一段
	require.NoError(t, g.Create(&model.FileCode{Code: "A3", FilePath: "uploads/2026/10/26/c.tar.gz", Text: "包.tar.gz"}).Error)
	// 无原始名 ⇒ 回退 file_path 末段
	require.NoError(t, g.Create(&model.FileCode{Code: "A4", FilePath: "uploads/2026/10/26/up_x.png"}).Error)
	// 文本分享（无文件路径）⇒ 不参与
	require.NoError(t, g.Create(&model.FileCode{Code: "T1", Text: "一段正文"}).Error)
	// 无扩展名 ⇒ 不计
	require.NoError(t, g.Create(&model.FileCode{Code: "A5", FilePath: "uploads/2026/10/26/readme", Text: "README"}).Error)

	rows, err := repo.TopSuffixes(ctx, 10)
	require.NoError(t, err, "统计不得静默失败")
	require.NotEmpty(t, rows)

	got := map[string]int64{}
	for _, r := range rows {
		got[r.Suffix] = r.Count
	}
	assert.Equal(t, int64(2), got[".pdf"], "两个 .pdf 原始名应合并计数")
	assert.Equal(t, int64(1), got[".gz"], "多点扩展名取最后一段")
	assert.Equal(t, int64(1), got[".png"], "无原始名时回退 file_path 末段")
	assert.NotContains(t, got, ".", "无扩展名的行不计入")
	// 文本分享不进统计
	for _, r := range rows {
		assert.NotEqual(t, ".一段正文", r.Suffix)
	}
}

// TestTopSuffixes_LimitAndOrder 回归：TOP N 按计数降序、同数按扩展名稳定排序。
func TestTopSuffixes_LimitAndOrder(t *testing.T) {
	g := newSuffixStatsDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	require.NoError(t, g.Create(&model.FileCode{Code: "B1", FilePath: "uploads/x.pdf", Text: "a.pdf"}).Error)
	require.NoError(t, g.Create(&model.FileCode{Code: "B2", FilePath: "uploads/y.zip", Text: "b.zip"}).Error)
	require.NoError(t, g.Create(&model.FileCode{Code: "B3", FilePath: "uploads/z.zip", Text: "c.zip"}).Error)

	rows, err := repo.TopSuffixes(ctx, 1)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, ".zip", rows[0].Suffix)
	assert.Equal(t, int64(2), rows[0].Count)
}
