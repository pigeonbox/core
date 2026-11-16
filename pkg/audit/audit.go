// Package audit 关键操作记录的单一执行点（core#173 第七片：Audit 收口）。
//
// ## 为什么有这个文件
//
// 管理操作审计（admin_operation_logs）此前有两个孪生写入实现：admin 域
// logAdminOperation 与 config 域 logConfigOperation 各自「提取操作者 + 构造
// 模型 + Create + 失败告警」抄一份（连失败告警文案都不一致）；gen handler
// 分别经 app/admin.Audit 与注入写者再落回其中一份。新域/新 handler 照抄
// 即可绕过收口点，且没有任何测试会红。
//
// ⇒ 2026-11-16 收口于此：Record 是关键操作记录的唯一执行点——提取操作者
// （middleware.AuditActor 单源）→ 构造 Entry → Sink 落盘（bootstrap 以 dao
// 桥接，同构 pkg/transfer）。域服务与 handler 只准调 audit.Record，不得裸
// 构造 model.AdminOperationLog。
//
// 行为零变化：
//
//   - Entry 与既有审计行的写入列一一对应（Action/Target/Success/ActorID/
//     ActorName/IP）；Message/LatencyMs 现无任何写入方，保持零值，不进本结构
//     （加列/填值即行为变化，须走设计）；
//   - 同步落盘、失败只记运行日志不阻断业务——与收口前两处实现的降级口径一致
//     （失败告警文案统一为 "audit write failed"，原两处文案不同且无断言）；
//   - 查询面（/admin/activities 分页过滤口径）与审计行语义不动。
//
// 请求关联：X-Trace-Id → context → 运行日志由 pkg/middleware.RequestID 承担，
// 运行日志经 logger.CtxWarn 自动带 trace_id；审计行不加 trace 列（行为零变化）。
//
// 脱敏：本包只接受调用方给定的 action/target 短文案，不提供把请求体/配置体/
// 密钥写进审计的任何路径；target 拼接由各调用点负责（现有点均无明文口令/令牌/
// 密钥——user.reset_password 只记 "user %d password reset"，config.update 记
// 固定文案与错误消息）。
//
// 守卫：bootstrap/audit_entry_guard_test.go——写入站点必须登记并含执行点调用
// 标记；源码裸构造 model.AdminOperationLog / 绕过执行点直写 ⇒ 测试红。
package audit

import (
	"context"

	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/pkg/middleware"
)

// Entry 一次关键操作记录（列与 model.AdminOperationLog 的写入列一一对应；
// Message/LatencyMs 现无写入方，不进本结构）。
type Entry struct {
	Action    string
	Target    string
	Success   bool
	ActorID   *uint
	ActorName string
	IP        string
}

// Sink 落盘能力（repo/db/dao 仓储实现，bootstrap 装配注入；pkg 是最底层
// 共享库，不直接依赖 repo 层——同构 pkg/transfer.Sink）。
type Sink interface {
	Create(ctx context.Context, e Entry) error
}

var sink Sink // nil = 未装配，记录丢弃并告警（装配错误的求救信号）

// SetSink 注入落盘实现（bootstrap 装配调用；须先于任何服务流量）。
func SetSink(s Sink) { sink = s }

// Record 记录一次关键操作：提取操作者（middleware.AuditActor 单源）→ 构造
// Entry → 同步落盘。sink 未装配记告警后丢弃；落盘失败只记运行日志，绝不阻断
// 业务（与收口前两处实现的降级口径一致）。
func Record(ctx context.Context, action, target string, success bool) {
	s := sink // 同步捕获（SetSink 须先于流量；与 pkg/transfer 同款）
	if s == nil {
		logger.CtxWarn(ctx, "audit sink not wired——审计记录丢弃", "action", action)
		return
	}
	actorID, actorName, ip := middleware.AuditActor(ctx)
	if err := s.Create(ctx, Entry{
		Action:    action,
		Target:    target,
		Success:   success,
		ActorID:   actorID,
		ActorName: actorName,
		IP:        ip,
	}); err != nil {
		logger.CtxWarn(ctx, "audit write failed", "action", action, "err", err)
	}
}
