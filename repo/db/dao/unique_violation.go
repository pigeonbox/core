package dao

// 唯一冲突判据（单源）：core 全仓只此一份判定「这次写入撞了唯一索引」。
//
// 背景：此前同一判定散在三处、且**口径不一**——
//   - repo/db/dao：手搓版（sqlite + mysql + pg 三家文案，窄判据）；
//   - app/share：两处**只认 gorm.ErrDuplicatedKey** ⇒ **sqlite 下漏判**
//     （core 默认库就是 sqlite ⇒ glebarez/sqlite 的唯一冲突既不映射
//     gorm.ErrDuplicatedKey，也不在文案匹配范围内）。
//
// ⇒ 收口到本函数：dao 与 app/share 共用同一判据，sqlite 路径不再漏判。
//
// 上游对应：oekit/gormx 的 IsUniqueViolation（v0.6.2 起补齐 sqlite 分支）。
// ⚠️ core 暂未直接依赖 oekit/gormx：该模块要求 gorm.io/gorm ≥ v1.31.2，
// 而 core 当前地板是 v1.25.12 ⇒ 采纳会把 gorm 地板抬两个 minor（独立评估项，
// 见 pigeonbox/core#6）。⇒ 本仓先自持一份，**判据与上游保持逐条一致**，
// 待 gorm 地板评估通过后再切上游（切换时本文件删除即可）。

import (
	"errors"
	"strings"

	"gorm.io/gorm"
)

// IsUniqueViolation 撞唯一键判据。
//
// 覆盖三种驱动的唯一冲突文案（**窄判据**：只匹配各家确切的唯一冲突措辞，
// 不加裸 "constraint failed" 之类的宽子串，避免把普通约束错误误判成唯一冲突）：
//
//   - sqlite（glebarez 纯 Go 驱动）："UNIQUE constraint failed: <表>.<列>"
//   - mysql："Error 1062: Duplicate entry …"
//   - pg："… duplicate key value violates unique constraint …"（23505）
//
// 另含 gorm.ErrDuplicatedKey 映射（需 gorm TranslateError，core 未开，
// 见 repo/db/database.go ⇒ mysql/pg 上该分支恒不命中）。
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || // sqlite
		strings.Contains(msg, "Duplicate entry") || // mysql 1062
		strings.Contains(msg, "duplicate key value") || // pg 文案
		strings.Contains(msg, "23505") // pg 错误码
}

// isSQLiteBusy SQLite 单写者 busy（含读快照过期后的 BUSY_SNAPSHOT 升级失败；
// 驱动报错文案不一，按子串匹配）。
func isSQLiteBusy(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "SQLITE_BUSY")
}
