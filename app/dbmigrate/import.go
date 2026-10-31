package dbmigrate

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/pigeonbox/core/repo/db/model"
)

// Importer 逻辑导入器（把行数据写进目标库）。
type Importer struct {
	db *gorm.DB
	// confirm 是否已显式确认（导入会写目标库 ⇒ 破坏性操作必须显式确认）。
	confirm bool
}

// NewImporter 创建导入器（目标库句柄）。
//
// ⚠️ `confirm=false` ⇒ 任何写操作都会失败（ErrImportNeedsConfirm）。
func NewImporter(db *gorm.DB, confirm bool) *Importer {
	return &Importer{db: db, confirm: confirm}
}

// ImportReport 一次导入的结果。
type ImportReport struct {
	// Tables 逐表结果（表名 + 导入行数）。
	Tables []TableImportResult
	// TotalRows 导入行数合计。
	TotalRows int
	// SequencesAligned 已对齐自增的表名清单。
	SequencesAligned []string
}

// TableImportResult 单表导入结果。
type TableImportResult struct {
	// Name 表名。
	Name string
	// Rows 导入的行数。
	Rows int
}

// Import 把 dump 的行数据导入目标库。
//
// # 顺序
//
//  1. **建表**：用 `model.AutoMigrateModels()` 在目标库按**目标方言**建表
//     （方言无关 ⇒ 不依赖那些 SQLite 方言的 migrations/*.sql）。
//  2. **逐表导入**：按 dump 里的表顺序（= 登记顺序 ⇒ 父表在前 ⇒ 外键不炸）。
//  3. **序列对齐**：把目标库的自增计数器重置到 `max(id)+1`
//     ⇒ **不做这一步，下一次新建行立刻撞主键**（PG 的 bigserial 序列
//     与 MySQL 的 AUTO_INCREMENT 都不会因为显式插入 ID 而前进）。
//
// # 幂等/可中断
//
// 每张表独立导入 ⇒ 失败时**已导入的表保持已导入**（可重跑；重跑前需清空目标表）。
func (i *Importer) Import(ctx context.Context, dump *Dump) (*ImportReport, error) {
	if i == nil || i.db == nil {
		return nil, fmt.Errorf("导入器未就绪（DB 未装配）")
	}
	if !i.confirm {
		return nil, ErrImportNeedsConfirm
	}
	if dump == nil {
		return nil, fmt.Errorf("导入数据为空")
	}
	if dump.DumpFormat != FormatVersion {
		return nil, fmt.Errorf("%w: %d（本版本只认 %d）", ErrUnsupportedDumpVersion, dump.DumpFormat, FormatVersion)
	}

	// 1) 建表（目标方言）。
	if err := i.db.WithContext(ctx).AutoMigrate(model.AutoMigrateModels()...); err != nil {
		return nil, fmt.Errorf("在目标库建表失败: %w", err)
	}

	report := &ImportReport{}

	// 2) 逐表导入。
	for _, td := range dump.Tables {
		n, err := i.importTable(ctx, td)
		if err != nil {
			return nil, fmt.Errorf("导入表 %s 失败（已导入 %d 行）: %w", td.Name, report.TotalRows, err)
		}
		report.Tables = append(report.Tables, TableImportResult{Name: td.Name, Rows: n})
		report.TotalRows += n
	}

	// 3) 序列对齐（**必须做**，否则下一次新建行撞主键）。
	aligned, err := i.alignSequences(ctx, dump)
	if err != nil {
		return nil, fmt.Errorf("对齐自增序列失败（**下一次新建行会撞主键**）: %w", err)
	}
	report.SequencesAligned = aligned

	return report, nil
}

// importTable 导入一张表。
func (i *Importer) importTable(ctx context.Context, td TableDump) (int, error) {
	if len(td.Rows) == 0 {
		return 0, nil
	}
	// 目标库的列类型与声明宽度（用于归一化 + 宽度校验）。
	colTypes, colWidths, err := i.columnTypes(ctx, td)
	if err != nil {
		return 0, err
	}
	// 宽度校验**先于**任何插入 ⇒ 失败时目标库**一条数据都没进去**
	// （不留在「插了一半」的半成品状态）。
	if err := checkWidths(td.Name, td.Columns, td.Rows, colWidths); err != nil {
		return 0, err
	}

	inserted := 0
	// 分批插入（大批量时避免超长 SQL / 参数个数上限）。
	const batch = 200
	for start := 0; start < len(td.Rows); start += batch {
		end := start + batch
		if end > len(td.Rows) {
			end = len(td.Rows)
		}
		for _, row := range td.Rows[start:end] {
			if len(row) != len(td.Columns) {
				return inserted, fmt.Errorf("行宽度与列数不符（列 %d，行 %d）", len(td.Columns), len(row))
			}
			values := make([]any, len(td.Columns))
			for ci, col := range td.Columns {
				values[ci] = normalizeValue(row[ci], colTypes[col])
			}
			if err := i.insertRow(ctx, td.Name, td.Columns, values); err != nil {
				return inserted, err
			}
			inserted++
		}
	}
	return inserted, nil
}

// insertRow 插入一行（显式 SQL + 占位符）。
//
// # 为什么不用 GORM 的 map 插入
//
// GORM 的 `Create(map[string]any{...})` 不保证**列序**（map 无序）⇒ 列多时
// 会错位；`Create([]map[string]any{...})` 的批量语义也随方言变化。
// ⇒ 这里直接用**显式列名 + 占位符**的 INSERT ⇒ 三个方言行为一致、列序确定。
//
// # 占位符方言差异（PG 用 $1…，SQLite/MySQL 用 ?）
//
// GORM 的 `db.Exec(sql, vars...)` 内部走 `clause.Expr.Build` → `stmt.AddVar`
// → **各方言的 BindVarTo** ⇒ PG 自动把 `?` 改写成 `$1…`
// ⇒ 这里**只写 `?`**，不自己拼占位符（自己拼 = 又一处方言分支 = 又一处漂移源）。
func (i *Importer) insertRow(ctx context.Context, table string, columns []string, values []any) error {
	quoted := make([]string, len(columns))
	for i2, c := range columns {
		quoted[i2] = i.db.Statement.Quote(c)
	}
	sql := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		i.db.Statement.Quote(table),
		strings.Join(quoted, ", "),
		strings.Join(placeholders(len(columns)), ", "),
	)
	return i.db.WithContext(ctx).Exec(sql, values...).Error
}

// placeholders 生成占位符列表（`?` 形态；PG 下由 GORM 的 BindVarTo 改写）。
func placeholders(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "?"
	}
	return out
}

// columnTypes 取目标库各列的数据类型（用于归一化）。
//
// ⚠️ 一并取出**声明宽度**（`size:N`）⇒ 导入前就能发现「模型声明的宽度
// 小于实际数据长度」⇒ **当场报人话**，而不是到目标库才炸成
// `value too long for type character varying(36)`（PG/MySQL 专有错误，
// SQLite 上永远看不到 ⇒ 只在换库时才暴露）。
//
// 真机实录（2026-10-31，10.10.10.3）：`upload_id` 声明 36、实际 64
// （由 file_hash 派生）⇒ **PG 上导入直接失败** ⇒ 定位到模型声明与实际值域不符。
func (i *Importer) columnTypes(ctx context.Context, td TableDump) (map[string]string, map[string]int, error) {
	types := make(map[string]string, len(td.Columns))
	widths := make(map[string]int, len(td.Columns))
	for _, c := range td.Columns {
		types[c] = "text"
	}
	stmt := &gorm.Statement{DB: i.db}
	if err := stmt.Parse(modelForTable(td.Name)); err != nil {
		// 目标库可能还没这张表的模型（理论上不会：导入前已 AutoMigrate）
		// ⇒ 退化成「全部按字符串处理 + 不校验宽度」。
		return types, widths, nil
	}
	for _, f := range stmt.Schema.Fields {
		if f.DBName == "" {
			continue
		}
		types[f.DBName] = string(f.DataType)
		if f.Size > 0 {
			widths[f.DBName] = f.Size
		}
	}
	return types, widths, nil
}

// checkWidths 校验各列的实际长度是否超出**模型声明的宽度**。
//
// # 为什么要有
//
// 「模型声明的宽度」是**换库目标库 schema 的宽度**（AutoMigrate 按模型建表）
// ⇒ 值超长 ⇒ 目标库插入失败（PG/MySQL：`value too long for type character varying(N)`）。
// 而 SQLite **忽略** varchar 长度 ⇒ 生产上跑得好好的 ⇒ **只有换库时才会炸**。
//
// ⇒ 在**导入前**逐列校验 ⇒ 失败时给出**人话**（哪张表、哪列、声明多宽、实际多长），
// 而不是让目标库抛晦涩的驱动错误。
func checkWidths(table string, columns []string, rows [][]*string, widths map[string]int) error {
	for ci, col := range columns {
		want, ok := widths[col]
		if !ok || want <= 0 {
			continue
		}
		maxLen := 0
		var sample string
		for _, r := range rows {
			if ci >= len(r) || r[ci] == nil {
				continue
			}
			if l := len(*r[ci]); l > maxLen {
				maxLen = l
				sample = *r[ci]
			}
		}
		if maxLen > want {
			return fmt.Errorf(
				"表 %s 的列 %s：数据最长 %d 字符，但模型只声明了 %d ⇒ 目标库建出的列装不下 ⇒ "+
					"请把该列的 size 调大（或确认真机数据确实超长）。样例: %q",
				table, col, maxLen, want, truncateSample(sample))
		}
	}
	return nil
}

// truncateSample 截断样例（避免错误信息过长）。
func truncateSample(s string) string {
	const max = 80
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// normalizeValue 把导出的值按**目标列类型**归一化。
//
// ⚠️ 这是跨方言的关键一步：
// SQLite 的布尔是 `numeric`（0/1），PG 的布尔是 `boolean` ⇒ 直接把 "0" 塞进
// PG 的 boolean 列会被拒（"invalid input syntax for type boolean"）。
//
// ⚠️ **NULL 必须还原成 NULL**（不是空串）：`user_id` 这类可空指针列，
// NULL（匿名上传）与空串是**不同语义** ⇒ nil 直传 nil。
//
// 归一化按**目标列的 Go 数据类型**（GORM 的 `DataType`）走：
//
//	bool    → true/false（接受 "1"/"0"/"true"/"false"/"t"/"f"…）
//	int     → int64
//	uint    → uint64
//	float   → float64
//	time    → time.Time（接受 RFC3339 与 "2006-01-02 15:04:05"）
//	string  → 原样
func normalizeValue(s *string, dataType string) any {
	if s == nil {
		// NULL 原样保留（不要变成空串）。
		return nil
	}
	switch dataType {
	case "bool":
		return parseBool(*s)
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64":
		n, err := strconv.ParseInt(strings.TrimSpace(*s), 10, 64)
		if err != nil {
			// 解析失败 ⇒ 原样给驱动（让驱动报真错误，而不是我们吞掉）。
			return *s
		}
		return n
	case "float32", "float64":
		f, err := strconv.ParseFloat(strings.TrimSpace(*s), 64)
		if err != nil {
			return *s
		}
		return f
	case "time":
		return parseTime(*s)
	default:
		return *s
	}
}

// parseBool 解析布尔值（宽容接受多种写法）。
func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "0", "false", "f", "no", "n":
		return false
	default:
		return true
	}
}

// parseTime 解析时间（RFC3339 优先，其次常见的无时区写法）。
func parseTime(s string) any {
	t := strings.TrimSpace(s)
	if t == "" {
		return nil
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if parsed, err := time.Parse(layout, t); err == nil {
			return parsed
		}
	}
	return s
}

// modelForTable 按表名找注册表里的模型（找不到 ⇒ nil）。
func modelForTable(name string) any {
	for _, spec := range model.Tables() {
		if spec.Name == name {
			return spec.Model
		}
	}
	return nil
}
