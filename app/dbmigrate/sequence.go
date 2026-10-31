package dbmigrate

import (
	"context"
	"fmt"

	"github.com/pigeonbox/core/repo/db/model"
)

// alignSequences 把目标库各表的**自增计数器**重置到 `max(id)+1`。
//
// # 为什么这是换库最经典的静默地雷
//
// 导入是**显式写 ID**（这样主键与外键保持不变 ⇒ 「取件码与链接不变」的承诺成立）。
// 但：
//
//   - Postgres 的 `bigserial` 背后的**序列**只被 `DEFAULT nextval()` 推进，
//     **显式插入 ID 时序列不动** ⇒ 序列仍停在 1 ⇒ 下一次 `INSERT` 不带 ID 时
//     拿到 1 ⇒ **撞主键**（而且是「迁移完第一次新建分享就炸」的那种）。
//   - MySQL 的 `AUTO_INCREMENT` 计数器在显式插入较大 ID 时**会**跟着抬
//     （InnoDB 的行为）⇒ 相对安全，但**不保证**（如 `INSERT ... SELECT`、
//     显式改小的 ID 等场景）⇒ 一样要显式重置，不能靠「MySQL 应该会自己管」。
//
// ⇒ 一律**显式重置**，不依赖任何一方的「默认行为」。
//
// # 各方言怎么重置
//
//	· postgres → `SELECT setval(pg_get_serial_sequence('t','id'), COALESCE(MAX(id), 1), MAX(id) IS NOT NULL) FROM t`
//	· mysql    → `ALTER TABLE t AUTO_INCREMENT = <max(id)+1>`
//	· sqlite   → 天然对齐（`INTEGER PRIMARY KEY` 取 `max(rowid)+1`）⇒ 跳过。
//
// # 语义细节
//
// - 空表 ⇒ 计数器设回 1（`setval(seq, 1, false)` ⇒ 下一次拿到 1）。
// - 非空表 ⇒ 计数器设到 `max(id)` 且 `is_called=true` ⇒ 下一次拿到 `max(id)+1`。
func (i *Importer) alignSequences(ctx context.Context, dump *Dump) ([]string, error) {
	driver := i.db.Name()
	if driver == "sqlite" {
		// SQLite 的 rowid 分配天然是 max(rowid)+1 ⇒ 无需（也无法）显式重置。
		return nil, nil
	}

	aligned := make([]string, 0, len(dump.Tables))
	for _, spec := range model.Tables() {
		if !spec.AutoIncrement {
			continue
		}
		// 只对**导入过的表**对齐（dump 里没有的表在目标库也不该有数据）。
		if !dumpHasTable(dump, spec.Name) {
			continue
		}
		if err := i.alignOne(ctx, driver, spec.Name); err != nil {
			return aligned, fmt.Errorf("表 %s 对齐失败: %w", spec.Name, err)
		}
		aligned = append(aligned, spec.Name)
	}
	return aligned, nil
}

// alignOne 对齐一张表的自增计数器。
func (i *Importer) alignOne(ctx context.Context, driver, table string) error {
	qTable := i.db.Statement.Quote(table)

	switch driver {
	case "postgres":
		// pg_get_serial_sequence 找不到序列时返回 NULL ⇒ setval(NULL,…) 会报错
		// ⇒ 先确认序列存在（表没有自增列时跳过，不是错误）。
		var seqName *string
		if err := i.db.WithContext(ctx).
			Raw("SELECT pg_get_serial_sequence(?, 'id')", table).
			Scan(&seqName).Error; err != nil {
			return fmt.Errorf("查询序列名失败: %w", err)
		}
		if seqName == nil || *seqName == "" {
			// 该表没有自增序列（正常路径下不该发生）⇒ 跳过。
			return nil
		}
		// setval(seq, max(id), true)：非空表 ⇒ 下一次拿到 max+1；
		// 空表 ⇒ COALESCE 兜底为 1 且 is_called=false ⇒ 下一次拿到 1。
		sql := fmt.Sprintf(
			"SELECT setval(pg_get_serial_sequence(?, 'id'), "+
				"COALESCE((SELECT MAX(id) FROM %s), 1), "+
				"(SELECT COUNT(*) > 0 FROM %s))",
			qTable, qTable)
		return i.db.WithContext(ctx).Exec(sql, table).Error

	case "mysql":
		// AUTO_INCREMENT 只能设成「当前 max+1」⇒ 先查 max。
		var maxID *int64
		if err := i.db.WithContext(ctx).
			Raw(fmt.Sprintf("SELECT MAX(id) FROM %s", qTable)).
			Scan(&maxID).Error; err != nil {
			return fmt.Errorf("查询 max(id) 失败: %w", err)
		}
		next := int64(1)
		if maxID != nil {
			next = *maxID + 1
		}
		return i.db.WithContext(ctx).
			Exec(fmt.Sprintf("ALTER TABLE %s AUTO_INCREMENT = %d", qTable, next)).
			Error

	default:
		return fmt.Errorf("不支持的数据库方言 %q（序列对齐只实现了 postgres / mysql）", driver)
	}
}

// dumpHasTable 判断 dump 里是否含某表。
func dumpHasTable(dump *Dump, name string) bool {
	for _, t := range dump.Tables {
		if t.Name == name {
			return true
		}
	}
	return false
}
