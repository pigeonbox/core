package bootstrap

import (
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/repo/db/model"
)

func newDataMigrateTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}, &model.FileCodeFile{}))
	return g
}

// TestRunDataMigrations_BackfillChildRows 回归：没有子表行的文件分享被补齐子行
// （虚拟行退役后，子表是文件清单的唯一真相源 ⇒ 存量必须先补齐）。
func TestRunDataMigrations_BackfillChildRows(t *testing.T) {
	g := newDataMigrateTestDB(t)

	// 文件分享：有路径、有原始名（Text）、无子表行
	require.NoError(t, g.Create(&model.FileCode{
		Code: "BACKFILL1", FilePath: "uploads/2026/10/25/x.txt", Text: "原始名.txt",
		Size: 42, ExpiredCount: -1,
	}).Error)
	// 文本分享：无路径 ⇒ 不该补
	require.NoError(t, g.Create(&model.FileCode{
		Code: "TEXTONLY1", Text: "正文内容", ExpiredCount: -1,
	}).Error)

	RunDataMigrations(g)

	var child model.FileCodeFile
	require.NoError(t, g.Where("file_code_id <> 0").First(&child).Error)
	assert.Equal(t, "uploads/2026/10/25/x.txt", child.FilePath)
	assert.Equal(t, "原始名.txt", child.FileName, "原始文件名取自主表 Text")
	assert.Equal(t, "x.txt", child.UUIDFileName, "存储名取路径末段")
	assert.Equal(t, int64(42), child.Size)

	var count int64
	require.NoError(t, g.Model(&model.FileCodeFile{}).Count(&count).Error)
	assert.Equal(t, int64(1), count, "文本分享不补子行")

	// 幂等：再跑一次不产生第二行
	RunDataMigrations(g)
	require.NoError(t, g.Model(&model.FileCodeFile{}).Count(&count).Error)
	assert.Equal(t, int64(1), count, "迁移必须幂等")
}

// TestRunDataMigrations_BackfillUsesPathNameWhenNoText 回归：主表没有原始名时，
// 子行文件名回退路径末段（否则取件页文件名为空）。
func TestRunDataMigrations_BackfillUsesPathNameWhenNoText(t *testing.T) {
	g := newDataMigrateTestDB(t)
	require.NoError(t, g.Create(&model.FileCode{
		Code: "BACKFILL2", FilePath: "uploads/2026/10/25/up_abc.bin", ExpiredCount: -1,
	}).Error)

	RunDataMigrations(g)

	var child model.FileCodeFile
	require.NoError(t, g.First(&child).Error)
	assert.Equal(t, "up_abc.bin", child.FileName)
}

// TestRunDataMigrations_NormalizeLegacyPathLayout 回归：「目录 + uuid 文件名」历史
// 布局被归一成完整相对路径（否则统一后的读路径会指向目录）。
func TestRunDataMigrations_NormalizeLegacyPathLayout(t *testing.T) {
	g := newDataMigrateTestDB(t)
	require.NoError(t, g.Create(&model.FileCode{
		Code: "NORM1", FilePath: "uploads/2026/10/25", UUIDFileName: "x.txt", ExpiredCount: -1,
	}).Error)
	// 已是完整路径形态 ⇒ 不动
	require.NoError(t, g.Create(&model.FileCode{
		Code: "NORM2", FilePath: "uploads/2026/10/25/y.txt", UUIDFileName: "y.txt", ExpiredCount: -1,
	}).Error)

	RunDataMigrations(g)

	var norm1, norm2 model.FileCode
	require.NoError(t, g.Where("code = ?", "NORM1").First(&norm1).Error)
	require.NoError(t, g.Where("code = ?", "NORM2").First(&norm2).Error)
	assert.Equal(t, "uploads/2026/10/25/x.txt", norm1.FilePath)
	assert.Equal(t, "uploads/2026/10/25/y.txt", norm2.FilePath)
}

// TestRunDataMigrations_SoftDeletedChildNotDuplicated 回归：「已有子行」判定必须
// 含软删行（2026-10-26 审计 P2）。
//
// 缺陷：实现用 GORM 默认作用域（**排除**软删行），而注释声明「含软删行也算
// 已存在」⇒ 两者相反 ⇒ 「主表存活但子行全软删」的分享会被迁移**补出一条活子行**
// （只对应第一个文件，物理文件可能已删）⇒ 凭空多一个 404 条目；且会掩盖
// 「恢复不恢复子行」的现场。
//
// 断言必须能变红（负对照）：把 Unscoped() 去掉，本用例即失败（活子行数 = 1）。
func TestRunDataMigrations_SoftDeletedChildNotDuplicated(t *testing.T) {
	g := newDataMigrateTestDB(t)

	// 主表存活（有路径）
	require.NoError(t, g.Create(&model.FileCode{
		Code: "SDCHILD1", FilePath: "uploads/2026/10/26/sd.txt", Text: "sd.txt",
		Size: 7, ExpiredCount: -1,
	}).Error)
	var parent model.FileCode
	require.NoError(t, g.Where("code = ?", "SDCHILD1").First(&parent).Error)

	// 子行 1 条，但**已软删**（回收站语义：分享被删、子行随之软删）
	child := model.FileCodeFile{
		FileCodeID: parent.ID, FileName: "sd.txt", UUIDFileName: "sd.txt",
		FilePath: "uploads/2026/10/26/sd.txt", Size: 7, SortOrder: 0,
	}
	require.NoError(t, g.Create(&child).Error)
	require.NoError(t, g.Delete(&model.FileCodeFile{}, child.ID).Error) // 软删

	RunDataMigrations(g)

	// Unscoped 计数不变（仍是 1 行）⇒ 迁移没有凭空补行
	var total int64
	require.NoError(t, g.Unscoped().Model(&model.FileCodeFile{}).
		Where("file_code_id = ?", parent.ID).Count(&total).Error)
	assert.Equal(t, int64(1), total, "迁移不得新增子行（软删子行也算「已存在」）")

	// 活子行数 = 0（软删行不得被迁移复活成活行）
	var alive int64
	require.NoError(t, g.Model(&model.FileCodeFile{}).
		Where("file_code_id = ?", parent.ID).Count(&alive).Error)
	assert.Equal(t, int64(0), alive, "软删子行不得被迁移补成活行")
}

// TestRunDataMigrations_DropRetiredColumns 回归：prefix/suffix 死列被删除。
func TestRunDataMigrations_DropRetiredColumns(t *testing.T) {
	g := newDataMigrateTestDB(t)
	// 模拟存量库：两列还在（恒空）
	require.NoError(t, g.Exec("ALTER TABLE file_codes ADD COLUMN prefix TEXT").Error)
	require.NoError(t, g.Exec("ALTER TABLE file_codes ADD COLUMN suffix TEXT").Error)

	RunDataMigrations(g)

	assert.False(t, columnExists(g, "file_codes", "prefix"), "prefix 死列应被删除")
	assert.False(t, columnExists(g, "file_codes", "suffix"), "suffix 死列应被删除")

	// 幂等：列已不存在时再跑不报错
	RunDataMigrations(g)
	assert.False(t, columnExists(g, "file_codes", "prefix"))
}
