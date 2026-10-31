// Package dbmigrate 换库（SQLite → Postgres/MySQL）：把一个实例的数据**逻辑**搬到另一个库。
//
// # 为什么是「逻辑导出/导入」而不是「物理拷贝」
//
// 备份包（app/backup）做的是**物理快照**：SQLite 的 `VACUUM INTO` 产出一个
// `.db` 文件，恢复时把它**换回去**。⇒ 这条路只在 SQLite → SQLite 之间成立：
// **Postgres/MySQL 根本读不了 SQLite 的文件格式**，反之亦然。
//
// 而「换库」的真需求恰恰是跨方言（SQLite → PG/MySQL）⇒ 必须走**逻辑**路线：
//
//	导出：把每张表读成**行数据**（与方言无关的 JSON）
//	导入：目标库按**目标方言**建表（GORM AutoMigrate）→ 逐行插入
//
// ⇒ 源库与目标库的方言在整条链路上**解耦**：SQLite→PG、PG→MySQL、MySQL→SQLite
// 全部同一条代码路径。
//
// # 三条语义（缺一条就危险）
//
//  1. **取件码与分享链接不变**：行数据**原样搬运**（含 `file_codes.id` 与 `code`）
//     ⇒ 用户的对外名片照旧可用。
//  2. **导入是破坏性操作**：它会写目标库 ⇒ 必须**显式确认**。
//  3. **导入后自增必须对齐**：导入是**显式写 ID**（为了让主键与外键保持不变），
//     而 PG 的 `bigserial` 序列 / MySQL 的 `AUTO_INCREMENT` **不会**因为显式插入
//     而前进 ⇒ 不重置的话，**下一次新建行立刻撞主键**。
//     ⇒ 这是换库最经典的静默地雷 ⇒ 本包把「序列对齐」做成**必经步骤**，
//     并有专门的守卫用例（见 import_test.go）。
//
// # 不搬什么
//
//   - `schema_migrations`：版本记录是**方言相关**的（SQLite 的版本号与 PG 的迁移集
//     没有对应关系）⇒ 目标库用 AutoMigrate 建表（方言无关），不搬版本记录。
//   - 存储对象：对象在**文件系统/对象存储**里，与数据库无关 ⇒ 换库不涉及。
package dbmigrate

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// FormatVersion 逻辑导出格式版本。
//
// 与备份包（app/backup）**各自独立**地演进：备份包是「物理快照 + 对象」，
// 本格式是「纯行数据」⇒ 两者的兼容性问题互不相干。
const FormatVersion = 1

// DumpName 逻辑导出文件（JSON）的建议文件名后缀。
const DumpName = "pigeonbox-db-dump.json"

// ErrUnsupportedDumpVersion 不认识的导出格式版本（宁可报错也不猜着还原）。
var ErrUnsupportedDumpVersion = fmt.Errorf("不认识的数据库导出格式版本")

// ErrImportNeedsConfirm 导入未显式确认（会写目标库 ⇒ 破坏性操作必须显式确认）。
var ErrImportNeedsConfirm = fmt.Errorf("导入会写入目标数据库，属破坏性操作：须显式确认")

// TableDump 一张表的行数据。
type TableDump struct {
	// Name 表名。
	Name string `json:"name"`
	// Columns 列名（按值顺序一一对应）。
	Columns []string `json:"columns"`
	// Rows 行数据；每行是 `Columns` 下标的**值数组**。
	//
	// ⚠️ 为什么用**字符串**而不是原生 JSON 值：
	// 数据库的值类型跨方言差异大（SQLite 的 `numeric` 布尔 vs PG 的 `boolean`；
	// SQLite 的 `datetime` 文本 vs PG 的 `timestamptz`）⇒ 用字符串承载「原始文本」，
	// 由**导入侧按目标列类型**归一化（见 import.go 的 normalizeValue）。
	// ⇒ 导出/导入两侧都不丢精度（整数不会被 JSON 浮点化）。
	//
	// ⚠️ **NULL 与空串必须可区分**：`user_id` 这类可空指针列，NULL（匿名上传）
	// 与空串是**不同语义** ⇒ 值用 `*string` 承载（JSON `null` vs `"…"`）。
	// （第一版用 `[]string` + 空串表示 NULL ⇒ 导入时把 NULL 变成了空串
	// ⇒ PG 的 `integer` 列收到 `""` 直接报错 —— 真机上必然踩。）
	Rows [][]*string `json:"rows"`
}

// Dump 一次逻辑导出的完整结果。
type Dump struct {
	// DumpFormat 格式版本（见 FormatVersion）。
	DumpFormat int `json:"dump_format"`
	// CreatedAt 导出时间（RFC3339）。
	CreatedAt string `json:"created_at"`
	// SourceDriver 源库方言（sqlite / postgres / mysql）。
	SourceDriver string `json:"source_driver"`
	// Tables 表数据（顺序 = `model.Tables()` 的登记顺序 ⇒ 父表在前）。
	Tables []TableDump `json:"tables"`
}

// TableNames 返回 dump 里包含的表名。
func (d *Dump) TableNames() []string {
	out := make([]string, 0, len(d.Tables))
	for _, t := range d.Tables {
		out = append(out, t.Name)
	}
	return out
}

// RowCount 返回 dump 里某表的行数（表不存在 ⇒ 0）。
func (d *Dump) RowCount(table string) int {
	for _, t := range d.Tables {
		if t.Name == table {
			return len(t.Rows)
		}
	}
	return 0
}

// TotalRows 返回全部表的行数合计。
func (d *Dump) TotalRows() int {
	n := 0
	for _, t := range d.Tables {
		n += len(t.Rows)
	}
	return n
}

// WriteJSON 把 dump 序列化成 JSON。
func (d *Dump) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(d)
}

// ReadDump 从 JSON 读回一个 dump（校验格式版本）。
func ReadDump(r io.Reader) (*Dump, error) {
	var d Dump
	if err := json.NewDecoder(r).Decode(&d); err != nil {
		return nil, fmt.Errorf("解析数据库导出文件失败: %w", err)
	}
	if d.DumpFormat != FormatVersion {
		return nil, fmt.Errorf("%w: %d（本版本只认 %d）", ErrUnsupportedDumpVersion, d.DumpFormat, FormatVersion)
	}
	return &d, nil
}

// nowRFC3339 当前时间（RFC3339）——抽出来便于测试注入。
func nowRFC3339() string {
	return time.Now().Format(time.RFC3339)
}
