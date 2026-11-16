package model

import "time"

// 对象 GC 任务状态机（gc_jobs.state，core#192）：
//
//	pending → running → done
//	   ↑         │
//	   │    ┌────┴─────────┐
//	   │    │ 失败：退避重试 │
//	   │    ▼              ▼
//	   │  pending      dead（attempts 耗尽；管理端可重放）
//	   │    ▲
//	   └────┘（running 且 lease_until 过期 ⇒ 其他 worker 可抢租约重跑）
//
// skipped：删前引用复核发现仍被引用 ⇒ 终态（「允许延迟，绝不误删」的底线）。
const (
	GCStatePending = "pending"
	GCStateRunning = "running"
	GCStateDone    = "done"
	GCStateSkipped = "skipped"
	GCStateDead    = "dead"
)

// GCJobReason 任务来源（gc_jobs.reason 取值）。
const (
	GCReasonPurge           = "purge"            // 回收站彻底删除（Transactional Outbox，core#192）
	GCReasonOrphanReconcile = "orphan_reconcile" // 孤儿对账
	GCReasonUserDeleted     = "user_deleted"     // 用户删除连带清理
)

// GCJob 对象回收持久化任务（core#192）。
//
// # 为什么需要
//
// 此前链路是「删库 → 尝试同步删对象 → 失败就等下一次全量对账」：
//   - 进程在「删库成功、删对象前」崩溃 ⇒ 只能靠全量对账兜底；远端后端
//     目前不删（report-only）⇒ 永久泄漏；
//   - 远端 5xx / 限流 / 超时没有重试租约，失败即丢；
//   - 对象被多 share 复用时没有统一「删前复核」，误删代价不对称；
//   - 积压不可观测。
//
// ⇒ gc_jobs 是**持久化任务面**：与业务删除同事务写入（Transactional Outbox），
// worker 按租约消费；崩溃/重启/远端抖动都不丢任务。多副本只有一个 worker
// 跑（bootstrap 仅 admin 面装配）+ 行级 CAS 抢占，天然幂等安全。
//
// 刻意**不带 gorm.Model 的 DeletedAt**：任务行没有软删语义（状态机即生命周期），
// 软删列只会让查询处处要记 Unscoped。
type GCJob struct {
	// ID 主键。
	ID uint `gorm:"primaryKey" json:"id"`
	// CreatedAt / UpdatedAt gorm 自动维护。
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// ObjectKey 存储相对路径（与 file_codes.file_path / file_code_files.file_path
	// 同一空间——删前引用复核按它查 CountFilePathReferences）。
	ObjectKey string `gorm:"size:512;index:idx_gc_jobs_state_next,priority:2" json:"object_key"`
	// Backend 入队时的后端类型（local/s3/webdav/...，观测用；执行期以**当前
	// 激活后端**为准——换后端后仍按当前实例的 key 空间删）。
	Backend string `gorm:"size:32" json:"backend"`
	// Reason 任务来源（GCJobReason*）。
	Reason string `gorm:"size:64" json:"reason"`
	// State 状态机（GCState*；pending|running|done|skipped|dead）。
	State string `gorm:"size:16;default:'pending';index:idx_gc_jobs_state_next,priority:1" json:"state"`
	// Attempts 已执行次数（CAS 抢占时 +1）。
	Attempts int `gorm:"default:0" json:"attempts"`
	// NextAttemptAt 退避调度：pending 态在此时刻之前不被认领。
	NextAttemptAt time.Time `gorm:"index:idx_gc_jobs_state_next,priority:3" json:"next_attempt_at"`
	// LeaseUntil running 态租约到期时刻；过期即视为持有者死亡，可被抢占。
	LeaseUntil *time.Time `json:"lease_until"`
	// LastError 最近一次失败原因（done 前的失败留痕 / skipped 的跳过原因）。
	LastError string `gorm:"type:text" json:"last_error,omitempty"`
}

// IsTerminal 该任务是否已到终态（不再被认领）。
func (j *GCJob) IsTerminal() bool {
	switch j.State {
	case GCStateDone, GCStateSkipped, GCStateDead:
		return true
	}
	return false
}
