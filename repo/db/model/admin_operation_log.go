package model

import "gorm.io/gorm"

// AdminOperationLogTargetMaxLen admin_operation_logs.target 列的字符上限。
//
// ⚠️ 必须与下面 Target 字段 gorm tag 的 size 一致（admin_operation_log_test.go 守卫）——
// target 是**人拼的字符串**：配置写入审计会拼「本次改动的键名清单」，长度随本次
// 改动字段数增长（50 个键名 ≈ 555 字符）⇒ 列宽是**硬约束**：
//
//	· PG/MySQL 严格模式：超长直接写不进（value too long for type character varying(N)）；
//	· 换库导入（app/dbmigrate/import.go）：按「数据最长 vs 模型声明」逐列预检
//	  ⇒ 超长即拒导（2026-11-17 实录：S26b 换库导入红）。
//
// SQLite 不校验 varchar 长度 ⇒ 生产跑得好好的，**只有换库时才炸**。
//
// ⇒ 写入方（app/config 的审计 target 组装）按此上限在**源头**封顶。
const AdminOperationLogTargetMaxLen = 255

// AdminOperationLog 记录后台运维操作
// Action: 具体操作标识，例如 maintenance.clean_temp
// Target: 操作影响的目标对象描述（长度上限见 AdminOperationLogTargetMaxLen）
// Success: 操作是否成功
// Message: 可读的描述信息
// Actor: 操作者信息
// LatencyMs: 操作耗时（毫秒，可选）
type AdminOperationLog struct {
	gorm.Model
	Action    string `gorm:"size:100;index" json:"action"`
	Target    string `gorm:"size:255" json:"target"`
	Success   bool   `json:"success"`
	Message   string `gorm:"type:text" json:"message"`
	ActorID   *uint  `gorm:"index" json:"actor_id"`
	ActorName string `gorm:"size:100" json:"actor_name"`
	IP        string `gorm:"size:45" json:"ip"`
	LatencyMs int64  `json:"latency_ms"`
}

// AdminOperationLogQuery 查询后台操作日志
// Success: nil 表示忽略； true/false 表示按结果过滤
type AdminOperationLogQuery struct {
	Action   string
	Actor    string
	Success  *bool
	Page     int
	PageSize int
}
