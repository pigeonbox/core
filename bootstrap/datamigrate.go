// datamigrate.go — 启动期数据迁移（2026-10-26 兼容层清理）。
//
// 为什么要 Go 侧迁移而不是 migrations/*.sql：
// 版本化迁移只在 database.migrate=true 时执行；默认路径走 GORM AutoMigrate，
// 而 AutoMigrate **不会**做数据回填（也不会删列）。本轮退役的兼容层里有
// 「存量数据必须先补齐，代码才能删掉兜底」的部分（子表回填），必须两条路径
// 都生效 ⇒ 实现为幂等的 Go 侧迁移。
//
// 全部步骤都幂等：可重复执行，失败只记日志不阻断启动（数据类迁移失败不应
// 让站点起不来；但读取侧会因缺数据而报错，故日志必须响亮）。
package bootstrap

import (
	"fmt"
	"strings"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/repo/db/model"
)

// RunDataMigrations 启动期数据迁移入口（幂等）。
//
// 调用时机：InitDatabase 之后（AutoMigrate 已补齐表结构）、对外服务之前
// （读取侧必须先于迁移完成——子表是文件清单的唯一真相源）。
func RunDataMigrations(gormDB *gorm.DB) {
	if gormDB == nil {
		return
	}
	normalizeFilePathLayout(gormDB)
	backfillShareFileRows(gormDB)
	dropRetiredFileCodeColumns(gormDB)
}

// normalizeFilePathLayout 归一「目录 + UUID 文件名」历史布局。
//
// 背景：file_codes.file_path 历史上存在两种落库形态——「完整相对路径」与
// 「目录 + uuid_file_name 另存文件名」。本轮把列语义统一为「完整相对路径」
// （GetFilePath 退化为单一取值）⇒ 存量里仍是「目录」形态的行必须先拼上文件名，
// 否则读路径按「完整相对路径」解析会指向目录（下载 404）。
//
// 判定：file_path 非空、uuid_file_name 非空、且 file_path 末段 ≠ uuid_file_name
// ⇒ 视为目录形态，拼接文件名（"/" 分隔，与 rel 路径一致）。
func normalizeFilePathLayout(gormDB *gorm.DB) {
	type row struct {
		ID           uint
		FilePath     string
		UUIDFileName string
	}
	var rows []row
	if err := gormDB.Model(&model.FileCode{}).
		Where("file_path <> '' AND uuid_file_name <> ''").
		Find(&rows).Error; err != nil {
		logger.Error("data migration: query file path rows failed", zap.Error(err))
		return
	}
	fixed := 0
	for _, r := range rows {
		if strings.HasSuffix(strings.TrimRight(r.FilePath, "/"), "/"+r.UUIDFileName) ||
			strings.TrimRight(r.FilePath, "/") == r.UUIDFileName {
			continue // 已是完整路径形态
		}
		newPath := strings.TrimRight(r.FilePath, "/") + "/" + r.UUIDFileName
		if err := gormDB.Model(&model.FileCode{}).Where("id = ?", r.ID).
			UpdateColumn("file_path", newPath).Error; err != nil {
			logger.Error("data migration: normalize file path failed",
				zap.Uint("id", r.ID), zap.String("file_path", newPath), zap.Error(err))
			continue
		}
		fixed++
	}
	if fixed > 0 {
		logger.Info("data migration: normalized legacy file path layout", zap.Int("rows", fixed))
	}
}

// backfillShareFileRows 为「有文件路径但没有子表行」的存量分享补齐子表行。
//
// 背景：多文件分享改造后，file_code_files 子表是文件清单的唯一真相源；旧单文件
// 分享没有子表行，取件端此前靠主表合成「虚拟行（id=0）」兜底。本轮退役该虚拟
// 行 ⇒ 存量数据必须先补齐，否则老分享的取件页会显示空文件列表。
//
// 补齐规则：file_name = 原始文件名（Text，空则取路径末段）、uuid_file_name =
// 路径末段（存储名）、file_path = 主表路径、size/file_hash 随主表。
func backfillShareFileRows(gormDB *gorm.DB) {
	var parents []model.FileCode
	if err := gormDB.Where("file_path <> '' AND deleted_at IS NULL").Find(&parents).Error; err != nil {
		logger.Error("data migration: query file shares failed", zap.Error(err))
		return
	}
	if len(parents) == 0 {
		return
	}

	// 已有子行（含软删行也算「已存在」：软删子行是回收站语义，不重复补）
	//
	// ⚠️ 2026-10-26 审计修复（P2）：实现此前用 GORM **默认作用域**（排除软删行）
	// ⇒ 与上方注释相反 ⇒ 「主表存活但子行全软删」的分享会被迁移**补出一条活子行**
	// （只对应第一个文件，物理文件可能已删）⇒ 凭空多一个 404 条目；且会掩盖
	// 「恢复不恢复子行」的现场。
	// ⇒ Unscoped()：软删行同样算「已存在」（与注释一致）。
	var covered []uint
	if err := gormDB.Unscoped().Model(&model.FileCodeFile{}).
		Distinct().Pluck("file_code_id", &covered).Error; err != nil {
		logger.Error("data migration: query existing child rows failed", zap.Error(err))
		return
	}
	has := make(map[uint]bool, len(covered))
	for _, id := range covered {
		has[id] = true
	}

	rows := make([]model.FileCodeFile, 0, len(parents))
	for i := range parents {
		p := &parents[i]
		if has[p.ID] {
			continue
		}
		name := baseNameOfPath(p.FilePath)
		original := strings.TrimSpace(p.Text)
		if original == "" {
			original = name
		}
		rows = append(rows, model.FileCodeFile{
			FileCodeID:   p.ID,
			FileName:     original,
			UUIDFileName: name,
			FilePath:     p.FilePath,
			Size:         p.Size,
			FileHash:     p.FileHash,
			SortOrder:    0,
		})
	}
	if len(rows) == 0 {
		return
	}
	if err := gormDB.CreateInBatches(&rows, 100).Error; err != nil {
		logger.Error("data migration: backfill share file rows failed", zap.Int("rows", len(rows)), zap.Error(err))
		return
	}
	logger.Info("data migration: backfilled share file rows",
		zap.Int("rows", len(rows)), zap.Int("file_shares", len(parents)))
}

// dropRetiredFileCodeColumns 删除已退役的死列。
//
// prefix/suffix 是旧直传布局的「原始名拆分」列：全库恒空、无任何写入方
// （真机 310 行全为空）。AutoMigrate 不会删列 ⇒ 这里显式 DROP。
func dropRetiredFileCodeColumns(gormDB *gorm.DB) {
	for _, col := range []string{"prefix", "suffix"} {
		if !columnExists(gormDB, "file_codes", col) {
			continue
		}
		// 列名来自上面的白名单常量，非外部输入
		stmt := fmt.Sprintf("ALTER TABLE file_codes DROP COLUMN %s", col)
		if err := gormDB.Exec(stmt).Error; err != nil {
			logger.Warn("data migration: drop retired column failed",
				zap.String("table", "file_codes"), zap.String("column", col), zap.Error(err))
			continue
		}
		logger.Info("data migration: retired column dropped",
			zap.String("table", "file_codes"), zap.String("column", col))
	}
}

// columnExists 列是否存在（跨驱动安全的探查：读 PRAGMA table_info）。
func columnExists(gormDB *gorm.DB, table, column string) bool {
	if !gormDB.Migrator().HasTable(table) {
		return false
	}
	rows, err := gormDB.Raw("SELECT name FROM pragma_table_info(?)", table).Rows()
	if err != nil {
		// pragma_table_info 是 SQLite 专有表值函数；非 SQLite 驱动退化为
		// Migrator.HasColumn（语义等价，只是要逐列探查）。
		return gormDB.Migrator().HasColumn(table, column)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false
		}
		if strings.EqualFold(name, column) {
			return true
		}
	}
	return false
}

// baseNameOfPath 相对路径末段（rel 恒用 "/" 分隔）。
func baseNameOfPath(rel string) string {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return ""
	}
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		return rel[i+1:]
	}
	return rel
}
