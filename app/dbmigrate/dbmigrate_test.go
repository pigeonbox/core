package dbmigrate

// dbmigrate 的端到端测试。
//
// # 测什么（按「会不会真出事」排序）
//
//  1. **导出→导入→数据一致**（行数与关键字段逐条比对）——这是换库的基本盘。
//  2. **自增对齐**：导入后**新建一行**必须成功（这是换库最经典的静默地雷：
//     显式写 ID 不推进 PG 的 bigserial 序列 ⇒ 不对齐的话「迁移完第一次
//     新建分享就撞主键」）。
//  3. **软删原样带走**：换库不是「只搬活数据」。
//  4. **类型归一化**：SQLite 的 numeric 布尔（0/1）不能直接塞进 PG 的 boolean。
//  5. **破坏性确认**：未确认 ⇒ 拒绝导入。
//  6. **格式版本**：不认识的版本 ⇒ 拒绝。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/pigeonbox/core/repo/db/model"
)

// newSQLite 开一个内存 SQLite（自动建表）。
func newSQLite(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(model.AutoMigrateModels()...))
	return db
}

// seedSource 往源库塞一批数据（覆盖布尔/时间/软删/空值几种形态）。
func seedSource(t *testing.T, db *gorm.DB) {
	t.Helper()
	now := time.Now().Truncate(time.Second)

	require.NoError(t, db.Create(&model.User{
		Username: "alice", Email: "alice@example.com", PasswordHash: "hash1",
		Role: "admin", Status: "active", EmailVerified: true,
		TotalUploads: 3, TotalDownloads: 5, TotalStorage: 1024,
	}).Error)

	require.NoError(t, db.Create(&model.User{
		Username: "bob", Email: "bob@example.com", PasswordHash: "hash2",
		Role: "user", Status: "active", EmailVerified: false,
	}).Error)

	// 软删用户（换库必须原样带走）。
	soft := model.User{Username: "carol", Email: "carol@example.com", PasswordHash: "h3"}
	require.NoError(t, db.Create(&soft).Error)
	require.NoError(t, db.Delete(&soft).Error)

	// 时间字段要在创建后赋值（内嵌 gorm.Model 的字段不能用在结构体字面量里）。
	fc := model.FileCode{
		Code: "ABC123", UUIDFileName: "u1.bin", FilePath: "uploads/2026/10/31/u1.bin",
		Size: 100, Text: "报告.pdf", Status: model.StatusNormal,
		ExpiredCount: -1, UsedCount: 2, FileHash: "sha256-1",
	}
	fc.CreatedAt = now
	fc.UpdatedAt = now
	require.NoError(t, db.Create(&fc).Error)

	require.NoError(t, db.Create(&model.FileCodeFile{
		FileCodeID: 1, FileName: "报告.pdf", UUIDFileName: "u1.bin",
		FilePath: "uploads/2026/10/31/u1.bin", Size: 100, FileHash: "sha256-1", SortOrder: 0,
	}).Error)
}

// TestExportImportRoundTrip —— 导出→导入→数据一致。
func TestExportImportRoundTrip(t *testing.T) {
	src := newSQLite(t)
	seedSource(t, src)

	ctx := context.Background()
	exp := NewExporter(src)
	dump, err := exp.Export(ctx)
	require.NoError(t, err)

	assert.Equal(t, FormatVersion, dump.DumpFormat)
	assert.Equal(t, "sqlite", dump.SourceDriver)
	assert.Positive(t, dump.TotalRows())

	// 导入到另一个库（同样是 SQLite ⇒ 但走的是**逻辑**路径，不是拷文件）。
	dst := newSQLite(t)
	imp := NewImporter(dst, true)
	report, err := imp.Import(ctx, dump)
	require.NoError(t, err)
	assert.Positive(t, report.TotalRows)

	// 逐表行数一致。
	for _, td := range dump.Tables {
		var n int64
		require.NoError(t, dst.Table(td.Name).Unscoped().Count(&n).Error)
		assert.Equalf(t, int64(len(td.Rows)), n, "表 %s 行数不一致", td.Name)
	}

	// 关键字段逐条比对（不是「拿得到 id」这种无牙断言）。
	var users []model.User
	require.NoError(t, dst.Unscoped().Order("id").Find(&users).Error)
	require.Len(t, users, 3)
	assert.Equal(t, "alice", users[0].Username)
	assert.Equal(t, "alice@example.com", users[0].Email)
	assert.True(t, users[0].EmailVerified, "布尔列必须原样带走")
	assert.Equal(t, 3, users[0].TotalUploads)
	assert.Equal(t, int64(1024), users[0].TotalStorage)
	assert.False(t, users[1].EmailVerified, "false 布尔不能变成 true")

	// 软删行原样带走。
	var carol model.User
	require.NoError(t, dst.Unscoped().Where("username = ?", "carol").First(&carol).Error)
	assert.True(t, carol.DeletedAt.Valid, "软删标记必须原样带走（换库不是只搬活数据）")

	// 分享记录：取件码与链接相关字段逐字一致。
	var fc model.FileCode
	require.NoError(t, dst.Unscoped().Where("code = ?", "ABC123").First(&fc).Error)
	assert.Equal(t, "ABC123", fc.Code)
	assert.Equal(t, "uploads/2026/10/31/u1.bin", fc.FilePath)
	assert.Equal(t, "报告.pdf", fc.Text)
	assert.Equal(t, int64(100), fc.Size)

	// 子表（外键指向 file_codes）。
	var fcf model.FileCodeFile
	require.NoError(t, dst.Unscoped().First(&fcf).Error)
	assert.Equal(t, uint(1), fcf.FileCodeID, "外键必须保持（ID 原样搬运）")
	assert.Equal(t, "报告.pdf", fcf.FileName)
}

// TestImportThenCreateDoesNotCollide —— **核心守卫**：导入后新建一行不得撞主键。
//
// 这是换库最经典的静默地雷：导入是**显式写 ID** ⇒ PG 的 bigserial 序列
// 与 MySQL 的 AUTO_INCREMENT 都不会前进 ⇒ 不对齐的话，迁移完**第一次新建
// 分享就撞主键**（而且是在生产环境第一次用到时才炸）。
//
// SQLite 的 rowid 天然对齐 ⇒ 本用例在 SQLite 上验证「数据与语义正确」；
// PG/MySQL 的序列对齐由 CI 的 service 容器用例覆盖（见 ci.yml 的
// 「换库（PG）」job）。
func TestImportThenCreateDoesNotCollide(t *testing.T) {
	src := newSQLite(t)
	seedSource(t, src)

	ctx := context.Background()
	dump, err := NewExporter(src).Export(ctx)
	require.NoError(t, err)

	dst := newSQLite(t)
	imp := NewImporter(dst, true)
	_, err = imp.Import(ctx, dump)
	require.NoError(t, err)

	// 导入后的最大 ID（新建**之前**取 ⇒ 才能断言新行避开了已用 ID）。
	var maxBefore uint
	require.NoError(t, dst.Model(&model.FileCode{}).Select("MAX(id)").Scan(&maxBefore).Error)

	// 导入后新建一行（不指定 ID ⇒ 走自增）。
	newCode := model.FileCode{
		Code: "NEW001", UUIDFileName: "new.bin", FilePath: "uploads/2026/10/31/new.bin",
		Size: 1, Text: "new.txt", Status: model.StatusNormal, ExpiredCount: -1,
	}
	require.NoError(t, dst.Create(&newCode).Error)
	assert.NotZero(t, newCode.ID, "自增必须给出新 ID")
	assert.Greaterf(t, newCode.ID, maxBefore,
		"自增必须避开已导入的 ID（否则就是撞主键）——这是换库最经典的静默地雷")
}

// TestImportRequiresConfirm —— 破坏性操作必须显式确认。
func TestImportRequiresConfirm(t *testing.T) {
	src := newSQLite(t)
	seedSource(t, src)
	dump, err := NewExporter(src).Export(context.Background())
	require.NoError(t, err)

	dst := newSQLite(t)
	imp := NewImporter(dst, false)
	_, err = imp.Import(context.Background(), dump)
	require.ErrorIs(t, err, ErrImportNeedsConfirm)

	// 拒绝后目标库必须**一条数据都没进去**（拒绝要发生在写之前）。
	var n int64
	require.NoError(t, dst.Model(&model.FileCode{}).Count(&n).Error)
	assert.Zero(t, n)
}

// TestDumpFormatVersionRejected —— 不认识的格式版本必须拒绝。
func TestDumpFormatVersionRejected(t *testing.T) {
	dump := &Dump{DumpFormat: FormatVersion + 99}
	_, err := ReadDump(bytesReader(marshalJSON(t, dump)))
	require.ErrorIs(t, err, ErrUnsupportedDumpVersion)
}

// TestExportEmptyDB —— 空库导出/导入要正常（不能因为 0 行而崩）。
func TestExportEmptyDB(t *testing.T) {
	src := newSQLite(t)
	dump, err := NewExporter(src).Export(context.Background())
	require.NoError(t, err)
	assert.Zero(t, dump.TotalRows())

	dst := newSQLite(t)
	report, err := NewImporter(dst, true).Import(context.Background(), dump)
	require.NoError(t, err)
	assert.Zero(t, report.TotalRows)
}

// TestNormalizeValue —— 类型归一化（跨方言的关键一步）。
func TestNormalizeValue(t *testing.T) {
	// 布尔：SQLite 的 numeric（0/1）⇒ PG 的 boolean。
	assert.Equal(t, true, normalizeValue(sp("1"), "bool"))
	assert.Equal(t, false, normalizeValue(sp("0"), "bool"))
	assert.Equal(t, true, normalizeValue(sp("true"), "bool"))
	assert.Equal(t, false, normalizeValue(sp("false"), "bool"))
	assert.Equal(t, false, normalizeValue(sp(""), "bool"))

	// 整数。
	assert.Equal(t, int64(42), normalizeValue(sp("42"), "int"))
	assert.Equal(t, int64(-7), normalizeValue(sp("-7"), "int64"))

	// 浮点。
	assert.Equal(t, 1.5, normalizeValue(sp("1.5"), "float64"))

	// 时间。
	got := normalizeValue(sp("2026-10-31T08:00:00Z"), "time")
	ts, ok := got.(time.Time)
	require.True(t, ok, "时间列必须解析成 time.Time（PG 的 timestamptz 要真时间）")
	assert.Equal(t, 2026, ts.Year())

	// 字符串原样。
	assert.Equal(t, "hello", normalizeValue(sp("hello"), "string"))
}

// bytesReader 把字节切片变成 reader（测试辅助）。
func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// marshalJSON 序列化成字节（测试辅助）。
func marshalJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

// sp 取可空字符串指针（测试辅助）。
func sp(s string) *string { return &s }
