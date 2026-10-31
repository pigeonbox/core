package dbmigrate

// **真 MySQL 验收**（MySQL 与 PG 的差异必须真库验，不能推断）。
//
// # MySQL 与 PG 的三处实质差异（换库必须分别处理）
//
//  1. **AUTO_INCREMENT 行为**：InnoDB 在显式插入较大 ID 时**通常**会自己把计数器
//     抬上去 ⇒ 比 PG「安全」⇒ 但**不保证**（`INSERT ... SELECT`、显式改小的 ID
//     等场景）⇒ 本包一律**显式重置**（见 sequence.go）⇒ 不依赖任何「默认行为」。
//  2. **字符集**：默认 `latin1` ⇒ 中文会变成 `?` ⇒ 必须显式 `utf8mb4`。
//  3. **布尔**：`tinyint(1)`（不是真 boolean）⇒ 归一化成 bool 后由驱动写 0/1。
//
// # 怎么跑
//
//	PB_TEST_MYSQL_DSN="pigeonbox:pbtest@tcp(127.0.0.1:53306)/pigeonbox_test?charset=utf8mb4&parseTime=True&loc=Local" \
//	  go test ./app/dbmigrate/ -run TestMySQL -v

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/pigeonbox/core/repo/db/model"
)

// mysqlDSNEnv 环境变量名（真 MySQL 的 DSN）。
const mysqlDSNEnv = "PB_TEST_MYSQL_DSN"

// openMySQL 开一个真 MySQL 连接（未设环境变量 ⇒ 跳过）。
func openMySQL(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv(mysqlDSNEnv)
	if dsn == "" {
		t.Skipf("未设 %s ⇒ 跳过真 MySQL 验收（CI 的 mysql service 容器会设）", mysqlDSNEnv)
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	return db
}

// TestMySQLImportFromSQLite —— **真 MySQL**：SQLite 导出 → MySQL 导入。
func TestMySQLImportFromSQLite(t *testing.T) {
	ctx := context.Background()

	src := newSQLite(t)
	seedSource(t, src)
	dump, err := NewExporter(src).Export(ctx)
	require.NoError(t, err)

	my := openMySQL(t)
	dropAllMySQL(my)
	require.NoError(t, my.AutoMigrate(model.AutoMigrateModels()...))

	report, err := NewImporter(my, true).Import(ctx, dump)
	require.NoError(t, err, "SQLite → MySQL 导入必须成功")
	assert.Positive(t, report.TotalRows)

	for _, td := range dump.Tables {
		var n int64
		require.NoError(t, my.Table(td.Name).Unscoped().Count(&n).Error)
		assert.Equalf(t, int64(len(td.Rows)), n, "表 %s 在 MySQL 上的行数不一致", td.Name)
	}

	// 布尔列。
	var users []model.User
	require.NoError(t, my.Unscoped().Order("id").Find(&users).Error)
	require.Len(t, users, 3)
	assert.True(t, users[0].EmailVerified)
	assert.False(t, users[1].EmailVerified)

	// **UTF-8 中文必须逐字一致**（latin1 默认会把中文变成 ?）。
	var fc model.FileCode
	require.NoError(t, my.Unscoped().Where("code = ?", "ABC123").First(&fc).Error)
	assert.Equal(t, "报告.pdf", fc.Text, "UTF-8 内容在 MySQL 上必须逐字一致（字符集问题）")
	assert.Equal(t, "uploads/2026/10/31/u1.bin", fc.FilePath)
	assert.Nil(t, fc.UserID, "可空列的 NULL 必须保持 NULL")
}

// TestMySQLSequenceAligned —— **真 MySQL**：导入后新建行不撞主键。
func TestMySQLSequenceAligned(t *testing.T) {
	ctx := context.Background()

	src := newSQLite(t)
	seedSource(t, src)
	dump, err := NewExporter(src).Export(ctx)
	require.NoError(t, err)

	my := openMySQL(t)
	dropAllMySQL(my)
	require.NoError(t, my.AutoMigrate(model.AutoMigrateModels()...))

	_, err = NewImporter(my, true).Import(ctx, dump)
	require.NoError(t, err)

	var maxBefore uint
	require.NoError(t, my.Model(&model.FileCode{}).Select("MAX(id)").Scan(&maxBefore).Error)
	assert.Positive(t, maxBefore)

	newCode := model.FileCode{
		Code: "MYNEW01", UUIDFileName: "mynew.bin", FilePath: "uploads/2026/10/31/mynew.bin",
		Size: 1, Text: "mynew.txt", Status: model.StatusNormal, ExpiredCount: -1,
	}
	require.NoError(t, my.Create(&newCode).Error,
		"导入后新建行必须成功（序列对齐的直接证据）")
	assert.Greaterf(t, newCode.ID, maxBefore, "新建行 ID 必须避开已导入的 ID")
}

// dropAllMySQL 清空目标库的所有表。
func dropAllMySQL(db *gorm.DB) {
	_ = db.Exec("SET FOREIGN_KEY_CHECKS = 0").Error
	for _, spec := range model.Tables() {
		_ = db.Exec("DROP TABLE IF EXISTS " + db.Statement.Quote(spec.Name)).Error
	}
	_ = db.Exec("SET FOREIGN_KEY_CHECKS = 1").Error
}
