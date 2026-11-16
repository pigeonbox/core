package bootstrap

// gcjobs.go —— 对象 GC 任务面（core#192）的管理面接线：
//   - 诊断段：gc_jobs 按状态计数 + 最老 pending 积压时长（/admin/diagnostics
//     一屏可见，「不知道」与「坏了」分开报——查询失败落 Error 不静默）；
//   - 死信重放端点：POST /admin/storage/gc-jobs/replay（dead → pending，
//     worker 下一轮重新消费；与 /admin/diagnostics 同一鉴权面 AdminOrAdminKey）。

import (
	"context"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/pigeonbox/contracts/errcode"
	"github.com/pigeonbox/core/pkg/resp"
	"github.com/pigeonbox/core/repo/db/dao"
)

// DiagnosticsGCJobs GC 任务积压快照（诊断 wire 形状；统计失败如实落 Error）。
type DiagnosticsGCJobs struct {
	// Pending 待消费（含到退避期未到的）。
	Pending int64 `json:"pending"`
	// Running 持租约执行中。
	Running int64 `json:"running"`
	// Done 已删除。
	Done int64 `json:"done"`
	// Skipped 引用复核命中跳过（仍被分享引用——「绝不误删」底线的可见证据）。
	Skipped int64 `json:"skipped"`
	// Dead 死信（attempts 耗尽；>0 需人工排查，可经 replay 端点重放）。
	Dead int64 `json:"dead"`
	// OldestPendingSeconds 最老 pending 的积压秒数（0=无积压任务）。
	OldestPendingSeconds int64 `json:"oldest_pending_seconds,omitempty"`
	// Error 采集失败原因（空=成功）。
	Error string `json:"error,omitempty"`
}

// diagnosticsGCJobs 采集 GC 任务积压（tasksDB 不可用/查询失败 ⇒ Error 可见）。
func diagnosticsGCJobs(ctx context.Context) DiagnosticsGCJobs {
	var out DiagnosticsGCJobs
	handle := tasksDB()
	if handle == nil {
		out.Error = "database not ready"
		return out
	}
	repo := dao.NewGCJobRepository(handle)
	counts, err := repo.CountByState(ctx)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Pending = counts["pending"]
	out.Running = counts["running"]
	out.Done = counts["done"]
	out.Skipped = counts["skipped"]
	out.Dead = counts["dead"]
	if age, ok, err := repo.OldestPendingAge(ctx, time.Now()); err == nil && ok {
		out.OldestPendingSeconds = int64(age.Seconds())
	}
	return out
}

// gcJobsReplayHandler 死信重放（POST /admin/storage/gc-jobs/replay，管理面）：
// 全部 dead 置回 pending（attempts 归零、立即到期），worker 下一轮重新消费。
// 返回重放条数；重放语义=「人工已排障（如远端恢复/配额调整），再给一次机会」。
func gcJobsReplayHandler(ctx context.Context, c *app.RequestContext) {
	handle := tasksDB()
	if handle == nil {
		resp.NewInternalError(c, errcode.CodeInternal, "gc replay: database not ready", nil)
		return
	}
	n, err := dao.NewGCJobRepository(handle).RequeueDead(ctx, time.Now())
	if err != nil {
		resp.NewInternalError(c, errcode.CodeInternal, "replay gc dead jobs", err)
		return
	}
	resp.Success(c, map[string]int64{"replayed": n})
}
