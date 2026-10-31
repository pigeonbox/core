package dbmigrate

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"gorm.io/gorm"

	"github.com/pigeonbox/core/repo/db/model"
)

// Exporter 逻辑导出器。
type Exporter struct {
	db *gorm.DB
}

// NewExporter 创建导出器（源库句柄）。
func NewExporter(db *gorm.DB) *Exporter {
	return &Exporter{db: db}
}

// Export 把源库的**全部领域模型表**导出成与方言无关的行数据。
//
// # 为什么按「模型」而不是「表名」导
//
// 逐表硬编码列名 ⇒ 每加一个字段就要改这里 ⇒ 迟早漂移（漏字段 = 换库后数据丢失）。
// ⇒ 这里用 **GORM 的 schema 反射**从模型推出「列名」，
// 导出时按 schema 的列顺序取值 ⇒ **模型改了，这里自动跟着改**。
//
// # 值怎么变成字符串
//
// 取原始值后按 Go 侧类型归一化成字符串：
//
//	· bool   → "true" / "false"
//	· 整数   → 十进制字符串（**不经 float** ⇒ 不丢精度）
//	· 时间   → RFC3339Nano
//	· []byte → 原样当字符串（TEXT 列）
//	· nil   → ""（导入侧由列的 nullable 语义区分 NULL 与空串）
func (e *Exporter) Export(ctx context.Context) (*Dump, error) {
	if e == nil || e.db == nil {
		return nil, fmt.Errorf("导出器未就绪（DB 未装配）")
	}

	dump := &Dump{
		DumpFormat:   FormatVersion,
		CreatedAt:    nowRFC3339(),
		SourceDriver: e.db.Name(),
	}

	for _, spec := range model.Tables() {
		td, err := e.exportTable(ctx, spec)
		if err != nil {
			return nil, fmt.Errorf("导出表 %s 失败: %w", spec.Name, err)
		}
		dump.Tables = append(dump.Tables, *td)
	}
	return dump, nil
}

// exportTable 导出一张表。
func (e *Exporter) exportTable(ctx context.Context, spec model.TableSpec) (*TableDump, error) {
	// 用 GORM 的 schema 反射拿列名。
	stmt := &gorm.Statement{DB: e.db}
	if err := stmt.Parse(spec.Model); err != nil {
		return nil, fmt.Errorf("解析模型 schema 失败: %w", err)
	}
	schema := stmt.Schema

	columns := make([]string, 0, len(schema.Fields))
	for _, f := range schema.Fields {
		if f.DBName == "" {
			continue // 忽略关联/虚拟字段
		}
		columns = append(columns, f.DBName)
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("模型 %T 没有可导出的列", spec.Model)
	}

	// 取数据：`Unscoped()` ⇒ 软删行**原样带走**（换库不是「只搬活数据」）。
	rows, err := e.db.WithContext(ctx).
		Table(spec.Name).
		Unscoped().
		Select(columns).
		Rows()
	if err != nil {
		return nil, fmt.Errorf("查询失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := &TableDump{Name: spec.Name, Columns: columns}
	for rows.Next() {
		vals := make([]any, len(columns))
		ptrs := make([]any, len(columns))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("读取行失败: %w", err)
		}
		row := make([]*string, len(columns))
		for i := range columns {
			row[i] = stringifyPtr(vals[i])
		}
		out.Rows = append(out.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历行失败: %w", err)
	}
	return out, nil
}

// stringifyPtr 把一个数据库原始值转成**可空字符串**。
//
// ⚠️ NULL 与空串必须可区分（`user_id` 的 NULL = 匿名上传，与空串语义不同）
// ⇒ nil 返回 nil（JSON `null`），其余返回指向字符串的指针。
//
// ⚠️ 精度：整数走 `int64`/`uint64` 分支（**不经 float**）⇒ 大整数不丢精度。
func stringifyPtr(v any) *string {
	if v == nil {
		return nil
	}
	s := stringify(v)
	return &s
}

// stringify 把一个数据库原始值转成字符串。
//
// ⚠️ 精度：整数走 `int64`/`uint64` 分支（**不经 float**）⇒ 大整数不丢精度。
func stringify(v any) string {
	switch val := v.(type) {
	case nil:
		return ""
	case bool:
		if val {
			return "true"
		}
		return "false"
	case int64:
		return fmt.Sprintf("%d", val)
	case int:
		return fmt.Sprintf("%d", val)
	case int32:
		return fmt.Sprintf("%d", val)
	case uint:
		return fmt.Sprintf("%d", val)
	case uint32:
		return fmt.Sprintf("%d", val)
	case uint64:
		return fmt.Sprintf("%d", val)
	case float64:
		// 浮点可能是整数列被驱动读成 float ⇒ 若是整数值就输出整数形态，
		// 免得导入侧把 "1" 写成 "1.0"。
		if val == float64(int64(val)) {
			return fmt.Sprintf("%d", int64(val))
		}
		return fmt.Sprintf("%g", val)
	case []byte:
		return string(val)
	case string:
		return val
	case time.Time:
		return val.Format(time.RFC3339Nano)
	default:
		// 其余类型（含 time.Time 的指针形态等）走 fmt。
		rv := reflect.ValueOf(v)
		if rv.Kind() == reflect.Pointer && !rv.IsNil() {
			elem := rv.Elem()
			if elem.Type() == reflect.TypeOf(time.Time{}) {
				return elem.Interface().(time.Time).Format(time.RFC3339Nano)
			}
		}
		return fmt.Sprintf("%v", val)
	}
}
