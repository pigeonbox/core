package bootstrap

import (
	"testing"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunDataMigrations_PurgesOrphanChildRows 启动期迁移必须清掉存量孤儿子行。
//
// 背景（core#62）：单条硬删路径 HardDeleteByCode 此前只删主表 ⇒ file_code_files
// 的子行永久残留（父行已不存在 = 孤儿）。真机已出现存量孤儿行。
// ⇒ 硬删路径修好后，存量孤儿需要一次性对账清理（幂等，启动期跑）。
//
// 口径：孤儿 = 子行的 file_code_id 在 file_codes 里**物理不存在**。
// 父行是软删（回收站）**不算**孤儿——它还可恢复。
func TestRunDataMigrations_PurgesOrphanChildRows(t *testing.T) {
	g := newDataMigrateTestDB(t)

	// ① 正常分享：父行存活 + 子行 ⇒ 必须保留
	require.NoError(t, g.Create(&model.FileCode{
		Code: "ORPH1", FilePath: "uploads/2026/11/04/a.txt", Text: "a.txt",
	}).Error)
	var alive model.FileCode
	require.NoError(t, g.Where("code = ?", "ORPH1").First(&alive).Error)
	require.NoError(t, g.Create(&model.FileCodeFile{
		FileCodeID: alive.ID, FileName: "a.txt", UUIDFileName: "a.txt",
		FilePath: "uploads/2026/11/04/a.txt",
	}).Error)

	// ② 回收站里的分享：父行软删 + 子行 ⇒ **不算**孤儿（可恢复）⇒ 必须保留
	require.NoError(t, g.Create(&model.FileCode{
		Code: "ORPH2", FilePath: "uploads/2026/11/04/b.txt", Text: "b.txt",
	}).Error)
	var trashed model.FileCode
	require.NoError(t, g.Where("code = ?", "ORPH2").First(&trashed).Error)
	require.NoError(t, g.Create(&model.FileCodeFile{
		FileCodeID: trashed.ID, FileName: "b.txt", UUIDFileName: "b.txt",
		FilePath: "uploads/2026/11/04/b.txt",
	}).Error)
	require.NoError(t, g.Delete(&model.FileCode{}, trashed.ID).Error)

	// ③ 旧 bug 留下的孤儿：父行物理没了，子行还在
	require.NoError(t, g.Create(&model.FileCode{
		Code: "ORPH3", FilePath: "uploads/2026/11/04/c.txt", Text: "c.txt",
	}).Error)
	var gone model.FileCode
	require.NoError(t, g.Where("code = ?", "ORPH3").First(&gone).Error)
	require.NoError(t, g.Create(&model.FileCodeFile{
		FileCodeID: gone.ID, FileName: "c.txt", UUIDFileName: "c.txt",
		FilePath: "uploads/2026/11/04/c.txt",
	}).Error)
	require.NoError(t, g.Unscoped().Delete(&model.FileCode{}, gone.ID).Error)

	RunDataMigrations(g)

	// 孤儿行必须被清掉
	var orphanLeft int64
	require.NoError(t, g.Unscoped().Model(&model.FileCodeFile{}).
		Where("file_code_id = ?", gone.ID).Count(&orphanLeft).Error)
	assert.Equal(t, int64(0), orphanLeft, "父行已物理删除的孤儿子行必须被清理")

	// 存活分享的子行必须保留
	var keepAlive int64
	require.NoError(t, g.Unscoped().Model(&model.FileCodeFile{}).
		Where("file_code_id = ?", alive.ID).Count(&keepAlive).Error)
	assert.Equal(t, int64(1), keepAlive, "存活分享的子行不得被误删")

	// 回收站里（父行软删）的子行必须保留（可恢复 ⇒ 不是孤儿）
	var keepTrashed int64
	require.NoError(t, g.Unscoped().Model(&model.FileCodeFile{}).
		Where("file_code_id = ?", trashed.ID).Count(&keepTrashed).Error)
	assert.Equal(t, int64(1), keepTrashed, "回收站内的子行不得被误删（还可恢复）")

	// 幂等：再跑一次结果不变
	RunDataMigrations(g)
	var total int64
	require.NoError(t, g.Unscoped().Model(&model.FileCodeFile{}).Count(&total).Error)
	assert.Equal(t, int64(2), total, "迁移必须幂等（重复执行不产生副作用）")
}
