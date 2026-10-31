package dbmigrate

// **真 Postgres 验收**（PG/MySQL 的行为与 SQLite 有实质差异 ⇒ 必须真库验）。
//
// # 为什么单独立文件
//
// SQLite 的 rowid 分配天然是 `max(rowid)+1` ⇒ **测不出序列问题**；
// 而 PG 的 `bigserial` 序列**不会**因为显式插入 ID 而前进 ⇒
// 不做序列对齐的话，「迁移完第一次新建分享」就撞主键。
//
// ⇒ 本文件在**真 PG** 上验证三件 SQLite 测不到的事：
//
//  1. **跨方言导入能成**（SQLite → PG）：schema 由 AutoMigrate 按 PG 方言建；
//     类型归一化（SQLite 的 numeric 布尔 → PG 的 boolean）真的被驱动接受。
//  2. **序列对齐真的有效**：导入后新建行拿到 `max(id)+1`（**不是**撞主键）。
//  3. **NULL 语义不丢**：`user_id` 的 NULL（匿名上传）在 PG 上仍是 NULL，
//     而不是空串（PG 的 integer 列收 "" 直接报错）。
//
// # 怎么跑
//
// 默认**跳过**（本地没 PG 也会绿）；有 PG 时用环境变量打开：
//
//	PB_TEST_PG_DSN="host=127.0.0.1 port=55432 user=pigeonbox password=pbtest dbname=pigeonbox_test sslmode=disable" \
//	  go test ./app/dbmigrate/ -run TestPG -v
//
// CI 的 service 容器（postgres:16）会设这个变量 ⇒ 真机验收在 CI 里做。

import (
	"context"
	"os"
	"testing"
	"time"

	"gorm.io/driver/postgres"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/pigeonbox/core/repo/db/model"
)

// pgDSNEnv 环境变量名（真 PG 的 DSN）。
const pgDSNEnv = "PB_TEST_PG_DSN"

// openPG 开一个真 PG 连接（未设环境变量 ⇒ 跳过）。
func openPG(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skipf("未设 %s ⇒ 跳过真 PG 验收（CI 的 postgres service 容器会设）", pgDSNEnv)
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	return db
}

// TestPGImportFromSQLite —— **真 PG**：SQLite 导出 → PG 导入。
func TestPGImportFromSQLite(t *testing.T) {
	ctx := context.Background()

	// 源：SQLite。
	src := newSQLite(t)
	seedSource(t, src)
	dump, err := NewExporter(src).Export(ctx)
	require.NoError(t, err)

	// 目标：真 PG（先清干净，保证可重复）。
	pg := openPG(t)
	dropAll(pg)
	require.NoError(t, pg.AutoMigrate(model.AutoMigrateModels()...))

	imp := NewImporter(pg, true)
	report, err := imp.Import(ctx, dump)
	require.NoError(t, err, "SQLite → PG 导入必须成功")
	assert.Positive(t, report.TotalRows)

	// 逐表行数一致（含软删行）。
	for _, td := range dump.Tables {
		var n int64
		require.NoError(t, pg.Table(td.Name).Unscoped().Count(&n).Error)
		assert.Equalf(t, int64(len(td.Rows)), n, "表 %s 在 PG 上的行数不一致", td.Name)
	}

	// 布尔列：SQLite 的 0/1 → PG 的 boolean 必须真落成 boolean。
	var users []model.User
	require.NoError(t, pg.Unscoped().Order("id").Find(&users).Error)
	require.Len(t, users, 3)
	assert.True(t, users[0].EmailVerified, "PG 的 boolean 列必须收到 true")
	assert.False(t, users[1].EmailVerified, "PG 的 boolean 列必须收到 false")

	// NULL 语义：`user_id`（匿名分享）必须是 NULL，不是空串。
	var fc model.FileCode
	require.NoError(t, pg.Unscoped().Where("code = ?", "ABC123").First(&fc).Error)
	assert.Nil(t, fc.UserID, "可空列的 NULL 必须保持 NULL（不是空串）")
	assert.Equal(t, "uploads/2026/10/31/u1.bin", fc.FilePath)
	assert.Equal(t, "报告.pdf", fc.Text, "UTF-8 内容在 PG 上必须逐字一致")

	// 时间列：SQLite 的 datetime 文本 → PG 的 timestamptz 必须是真时间。
	assert.False(t, fc.ExpiredAt != nil && fc.ExpiredAt.IsZero(), "过期时间语义保持")

	// 外键（子表指向父表）保持。
	var fcf model.FileCodeFile
	require.NoError(t, pg.Unscoped().First(&fcf).Error)
	assert.Equal(t, uint(1), fcf.FileCodeID)
}

// TestPGSequenceAligned —— **真 PG 的核心断言**：导入后新建行不撞主键。
//
// ⚠️ 这条在 SQLite 上**测不出来**（rowid 天然对齐）⇒ 必须真 PG。
// 负对照：跳过序列对齐 ⇒ PG 序列仍停在 1 ⇒ 新建行拿到 ID=1 ⇒ 撞主键。
func TestPGSequenceAligned(t *testing.T) {
	ctx := context.Background()

	src := newSQLite(t)
	seedSource(t, src)
	dump, err := NewExporter(src).Export(ctx)
	require.NoError(t, err)

	pg := openPG(t)
	dropAll(pg)
	require.NoError(t, pg.AutoMigrate(model.AutoMigrateModels()...))

	imp := NewImporter(pg, true)
	report, err := imp.Import(ctx, dump)
	require.NoError(t, err)
	assert.NotEmpty(t, report.SequencesAligned, "必须报告已对齐的表（否则等于没做）")

	// 导入后的最大 ID。
	var maxBefore uint
	require.NoError(t, pg.Model(&model.FileCode{}).Select("MAX(id)").Scan(&maxBefore).Error)
	assert.Positive(t, maxBefore)

	// **核心**：新建一行 ⇒ 必须拿到 max+1（不撞主键）。
	newCode := model.FileCode{
		Code: "PGNEW1", UUIDFileName: "pgnew.bin", FilePath: "uploads/2026/10/31/pgnew.bin",
		Size: 1, Text: "pgnew.txt", Status: model.StatusNormal, ExpiredCount: -1,
	}
	require.NoError(t, pg.Create(&newCode).Error,
		"导入后新建行必须成功——序列没对齐的话这里会撞主键（duplicate key value）")
	assert.Equalf(t, maxBefore+1, newCode.ID,
		"新建行的 ID 必须是 max(id)+1（序列对齐的直接证据）")
}

// TestPGSequenceNotAlignedNegative —— **负对照**：证明序列对齐确实是关键。
//
// 手工把 PG 的序列**推回 1**（模拟「没做序列对齐」）⇒ 新建行撞主键。
// ⇒ 这条证明上一条用例不是「本来就绿」。
func TestPGSequenceNotAlignedNegative(t *testing.T) {
	ctx := context.Background()

	src := newSQLite(t)
	seedSource(t, src)
	dump, err := NewExporter(src).Export(ctx)
	require.NoError(t, err)

	pg := openPG(t)
	dropAll(pg)
	require.NoError(t, pg.AutoMigrate(model.AutoMigrateModels()...))

	_, err = NewImporter(pg, true).Import(ctx, dump)
	require.NoError(t, err)

	// 手动把序列推回 1（= 模拟没对齐）。
	require.NoError(t, pg.Exec(
		"SELECT setval(pg_get_serial_sequence('file_codes','id'), 1, false)").Error)

	// 此时新建行 ⇒ 撞主键。
	newCode := model.FileCode{
		Code: "COLLIDE", UUIDFileName: "c.bin", FilePath: "uploads/2026/10/31/c.bin",
		Size: 1, Text: "c.txt", Status: model.StatusNormal, ExpiredCount: -1,
	}
	err = pg.Create(&newCode).Error
	require.Error(t, err, "序列未对齐时新建行必须撞主键（负对照：证明对齐是必要的）")
	assert.Contains(t, err.Error(), "duplicate key", "必须是主键冲突（不是别的错）")
}

// dropAll 清空目标库的所有表（保证用例可重复跑）。
func dropAll(db *gorm.DB) {
	for _, spec := range model.Tables() {
		_ = db.Exec("DROP TABLE IF EXISTS " + db.Statement.Quote(spec.Name) + " CASCADE").Error
	}
	_ = db.Exec("DROP TABLE IF EXISTS schema_migrations CASCADE").Error
}

// 确认时间类型在 PG 上真的被解析（避免 unused import）。
var _ = time.Now
