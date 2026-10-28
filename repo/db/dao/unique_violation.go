package dao

// 唯一冲突判据：**已收编上游 oekit/gormx**（core PR：抬 gorm 地板到 v1.31.2 后
// 直接依赖 `github.com/oekit/gormx`），本文件只保留**上游没有**的那一条：
// SQLite 单写者 busy 判定。
//
// ## 演进（别再回退）
//
// 1. 此前同一判定散在三处、且**口径不一**——
//   - repo/db/dao：手搓版（sqlite + mysql + pg 三家文案，窄判据）；
//   - app/share：两处**只认 gorm.ErrDuplicatedKey** ⇒ **sqlite 下漏判**
//     （core 默认库就是 sqlite ⇒ glebarez/sqlite 的唯一冲突既不映射
//     gorm.ErrDuplicatedKey，也不在文案匹配范围内）⇒ 换码重试形同虚设、
//     裸 SQL 报错漏给用户。
// 2. v0.32.20 收口到 dao 内的**单源**（本文件），sqlite 路径不再漏判。
// 3. 本次（gorm 地板抬到 v1.31.2）**进一步收编到上游**：`oekit/gormx`
//    v0.6.2 的 `IsUniqueViolation` 与本仓那份**逐条一致**（同样四个子串 +
//    `gorm.ErrDuplicatedKey` 映射）⇒ 直接用上游，删掉本仓重复实现，
//    避免两份窄判据并存漂移（窄判据靠错误文案匹配，改措辞即静默失配）。
//
//    上游对应实现见 github.com/oekit/gormx transaction.go:IsUniqueViolation。
//
// ⚠️ 收编的前提是 **gorm 地板 ≥ v1.31.2**（gormx 的地板）；抬地板的实测结论：
// glebarez/sqlite v1.11.0 在 gorm v1.31.2 下编译 + AutoMigrate + 唯一索引
// 冲突全部正常，core 全量测试零改动通过 ⇒ 地板抬升代价可接受。

import (
	"strings"
)

// isSQLiteBusy SQLite 单写者 busy（含读快照过期后的 BUSY_SNAPSHOT 升级失败；
// 驱动报错文案不一，按子串匹配）。
//
// 上游 oekit/gormx 未提供此判据（它只管唯一冲突）⇒ 留在本仓。
func isSQLiteBusy(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "SQLITE_BUSY")
}
