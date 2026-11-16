package db

import (
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// migrate_test 验证版本化迁移器的核心行为：
//   - 干净库首次迁移成功建表并记录版本
//   - 二次迁移幂等（不重复执行）
//   - 迁移后业务表存在

func newMigrateTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	return gormDB
}

// 测试：干净库首次迁移成功，schema_migrations 记录版本
func TestMigrator_Up_FreshDB(t *testing.T) {
	gormDB := newMigrateTestDB(t)
	m, err := NewMigrator(gormDB, "sqlite")
	require.NoError(t, err)

	applied, err := m.Up()
	require.NoError(t, err)
	assert.Equal(t, []string{"000001", "000002", "000003", "000004", "000005", "000006", "000007", "000008", "000009", "000010"}, applied)

	// 版本记录存在
	var versions []schemaMigrations
	require.NoError(t, gormDB.Find(&versions).Error)
	// 条数**不写死**（此前写死 6 ⇒ 每加一条迁移就要改测试，是个反复的税）：
	// 断言「记录数 = 本次应用的迁移数」，版本清单本身由上面的 applied 断言钉住。
	assert.Len(t, versions, len(applied), "版本记录数应与本次应用的迁移数一致")
	assert.Equal(t, "000001", versions[0].Version)
	assert.Equal(t, "000002", versions[1].Version)
	assert.Equal(t, "000003", versions[2].Version)
	assert.Equal(t, "000004", versions[3].Version)
	assert.Equal(t, "000005", versions[4].Version)
	assert.Equal(t, "000006", versions[5].Version)
	assert.Equal(t, "000007", versions[6].Version)
	assert.Equal(t, "000008", versions[7].Version)
}

// 测试：幂等性——二次 Up 不重复执行
func TestMigrator_Up_Idempotent(t *testing.T) {
	gormDB := newMigrateTestDB(t)
	m, _ := NewMigrator(gormDB, "sqlite")

	firstApplied, err := m.Up()
	require.NoError(t, err)

	// 二次迁移应返回空（无新迁移）
	applied, err := m.Up()
	require.NoError(t, err)
	assert.Empty(t, applied)

	// 版本记录条数不变（不写死数字 ⇒ 新增迁移不必再改这里）
	var count int64
	gormDB.Model(&schemaMigrations{}).Count(&count)
	assert.Equal(t, int64(len(firstApplied)), count)
}

// 测试：迁移后核心业务表存在
func TestMigrator_CreatesBusinessTables(t *testing.T) {
	gormDB := newMigrateTestDB(t)
	m, _ := NewMigrator(gormDB, "sqlite")
	_, err := m.Up()
	require.NoError(t, err)

	for _, table := range []string{"users", "file_codes", "upload_chunks", "transfer_logs", "admin_operation_logs", "user_api_keys"} {
		exists := gormDB.Migrator().HasTable(table)
		assert.True(t, exists, "table %s should exist after migration", table)
	}
}

// 测试：SQL 语句分割（含引号内分号）
func TestSplitSQLStatements_Quotes(t *testing.T) {
	sql := "CREATE TABLE a (x text); INSERT INTO a VALUES ('hello;world'); CREATE INDEX idx ON a(x);"
	stmts := splitSQLStatements(sql)
	// 应分割为 3 条（引号内的分号不切）
	assert.Len(t, stmts, 3)
	assert.Contains(t, stmts[1], "'hello;world'")
}

// 测试：空字符串与纯注释不产生语句
func TestSplitSQLStatements_EmptyAndComments(t *testing.T) {
	// 行注释整体被剥掉（含注释里的分号）⇒ 零语句。
	// 此前实现会把注释整块当成一条语句返回，靠 applyOne 的注释前缀守卫跳过——
	// 一旦注释与真语句之间没有分号，真语句也被连坐跳过（issue #184 实录）。
	assert.Empty(t, splitSQLStatements("-- 这是一条注释;\n"))
	assert.Empty(t, splitSQLStatements("-- 只有注释，无语句"))
}

// 测试：文件头注释 + 真语句 ⇒ 语句必须被切出来（不能被注释连坐吞掉）
func TestSplitSQLStatements_LeadingCommentThenStatement(t *testing.T) {
	sql := "-- 头注释，含一个分号;\n-- 第二行注释\nALTER TABLE `t` ADD COLUMN `c` text;\n"
	stmts := splitSQLStatements(sql)
	require.Len(t, stmts, 1)
	assert.Contains(t, stmts[0], "ALTER TABLE")
	assert.NotContains(t, stmts[0], "头注释")

	// 行尾注释不额外产生语句块（语句本身照常切出）
	tail := splitSQLStatements("ALTER TABLE t ADD COLUMN c text; -- 尾注释")
	require.Len(t, tail, 1)
	assert.Contains(t, tail[0], "ALTER TABLE")
}
