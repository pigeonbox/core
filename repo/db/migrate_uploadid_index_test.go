package db

import (
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/repo/db/model"
)

// 复现 issue #184：file_codes.upload_id 无索引 ⇒ 上传完成后的读回按 upload_id
// 查询走全表扫。两条建 schema 的路径都要有该索引：
//   1. 迁移路径（bootstrap 的 Migrator.Up，真相源=migrations/*.sql）
//   2. 模型路径（dbmigrate 换库导入用 model.AutoMigrateModels 建目标库）
//
// 索引必须是**非唯一**的：wave1 计划明令禁止「upload_id 唯一索引」
// （可为空/复用语义未定）。

const uploadIDIndexName = "idx_file_codes_upload_id"

// indexSQL 取 sqlite_master 里的建索引语句（判断唯一性用）。
func indexSQL(t *testing.T, gormDB *gorm.DB, name string) string {
	t.Helper()
	var sqlStr string
	err := gormDB.Raw("SELECT sql FROM sqlite_master WHERE type='index' AND name = ?", name).Scan(&sqlStr).Error
	require.NoError(t, err)
	require.NotEmpty(t, sqlStr, "index %s should exist in sqlite_master", name)
	return sqlStr
}

// queryPlan 取给定查询的 EXPLAIN QUERY PLAN 文本。
func queryPlan(t *testing.T, gormDB *gorm.DB, query string, args ...any) string {
	t.Helper()
	rows, err := gormDB.Raw("EXPLAIN QUERY PLAN "+query, args...).Rows()
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out strings.Builder
	cols, err := rows.Columns()
	require.NoError(t, err)
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		require.NoError(t, rows.Scan(ptrs...))
		for _, v := range vals {
			if s, ok := v.(string); ok {
				out.WriteString(s)
				out.WriteByte('\n')
			}
		}
	}
	require.NoError(t, rows.Err())
	return out.String()
}

// 测试：迁移路径建出非唯一索引，且 upload_id 读回走该索引（不再全表扫）。
func TestMigrator_FileCodesUploadIDIndex(t *testing.T) {
	gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	m, err := NewMigrator(gormDB, "sqlite")
	require.NoError(t, err)
	_, err = m.Up()
	require.NoError(t, err)

	require.True(t, gormDB.Migrator().HasIndex("file_codes", uploadIDIndexName),
		"file_codes.upload_id 应有索引 %s（issue #184）", uploadIDIndexName)

	sqlStr := indexSQL(t, gormDB, uploadIDIndexName)
	assert.NotContains(t, strings.ToUpper(sqlStr), "UNIQUE",
		"upload_id 可为空/复用语义未定 ⇒ 禁止唯一索引（wave1 计划明令项）")

	plan := queryPlan(t, gormDB, "SELECT * FROM file_codes WHERE upload_id = ?", "up-1")
	assert.Contains(t, plan, uploadIDIndexName,
		"按 upload_id 读回应走索引，不得全表扫；plan=%s", plan)
	assert.NotContains(t, plan, "SCAN file_codes",
		"按 upload_id 读回应走索引，不得全表扫；plan=%s", plan)
}

// 测试：模型路径（dbmigrate 换库导入按 AutoMigrateModels 建目标库）同样有该索引。
func TestModelSchema_FileCodesUploadIDIndex(t *testing.T) {
	gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, gormDB.AutoMigrate(model.AutoMigrateModels()...))

	require.True(t, gormDB.Migrator().HasIndex("file_codes", uploadIDIndexName),
		"AutoMigrate 建出的 file_codes 也应有 upload_id 索引（dbmigrate 换库路径）")

	plan := queryPlan(t, gormDB, "SELECT * FROM file_codes WHERE upload_id = ?", "up-1")
	assert.Contains(t, plan, uploadIDIndexName,
		"按 upload_id 读回应走索引，不得全表扫；plan=%s", plan)
}
