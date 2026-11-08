// queriespec.go 条件拼装收编 oekit/gormx/crud QuerySpec 的试点层（core#88）。
//
// 背景：dao 层查询普遍手拼 db.Where(...) 链，四类已知风险——空条件退化成
// 全表、多过滤拼装时占位符与参数漂移、外部可控字段名注入面、同一条
// *gorm.DB 链被并发追加。gormx/crud 的 Query/QuerySpec 把三个正合语义钉在
// 上游用例里：
//
//   - TestQuerySpec_FailClosed：字段不在标识符白名单、操作符不在白名单、
//     BETWEEN/IN 值形状非法 ⇒ 条件渲染为恒假 1=0，绝不静默丢弃（丢掉
//     租户/权限过滤 = 数据暴露面；1=0 只是查不到，不会越权）；
//   - TestQuerySpec_WhereGroup_Parenthesized：Or 组物化为单个带括号条件，
//     组内 OR 不击穿 AND 链（手拼最常见的越界 bug）；
//   - TestQuerySpec_SnapshotIsolation：Spec() 返回快照，物化后继续改构建器
//     不影响已取得的条件 ⇒ 构建器不再是可变共享态。
//
// 试点三处（core#88，只换条件拼装；Repository 接口、返回类型、分页口径、
// 排序全部零变化；EXPLAIN QUERY PLAN 逐条一致由 queriespec_pilot_test.go
// 用替换前录制的 golden plan 锁住）：
//
//  1. filecode_admin.go       ListWithFilter              管理台文件列表
//  2. filecode_usershares.go  GetUserSharesWithFilter     用户分享列表
//  3. transfer_log.go         TransferLogRepository.List  传输日志（审计面）
//
// 同模块零新增依赖边（gormx v0.11.7 已在钉版，gormrepo/crud 同 module）。
package dao

import (
	"github.com/oekit/gormx/crud"
	"github.com/oekit/gormx/gormrepo"
	"gorm.io/gorm"
)

// applySpecConds 把 crud.Query 的 WHERE 条件应用到 gorm 链上。
//
// 翻译复用上游 gormrepo.ApplyQuery：Query 不设 Page/Orders/Preloads 时它只落
// 条件（Conds 逐条 Where、OrConds 逐条 Or），排序仍由调用方 Order、分页仍由
// paginate 接管（core#8 的 Policy 口径不动）。条件文本由 crud 按「字段白名单 +
// 操作符白名单」生成并 fail-closed，这里不再存在静默丢弃路径。
//
// 拼装顺序契约：条件按 spec 内顺序逐条 Where（gorm MergeClause 保持追加序），
// gorm 的软删 scope 在语句构建期追加在 WHERE 末尾——与手拼时代的 WHERE 词序
// 一致。这是 EXPLAIN QUERY PLAN 逐条一致的前提（sqlite 按条件顺序做索引分析），
// 改动各 spec 构建函数的条件顺序前先跑 queriespec_pilot_test.go。
func applySpecConds(db *gorm.DB, q *crud.Query) *gorm.DB {
	return gormrepo.ApplyQuery(db, q)
}
