package bootstrap

// gcjobs_worker_fault_test.go —— 对象 GC 任务 worker 的 **存储故障注入**
// （L6 故障注入）。
//
// 注入什么故障：
//   GC 执行器依赖的存储（对象删除能力）不可用：deleter 返回错误
//   （等价远端后端 503 / 磁盘满 / 权限不足）；
//   另有一例注入 **DB 故障**（认领阶段读库失败）。
//
// 断言什么（全部是**行为**）：
//   ① 任务失败**被记录**：任务行落到 pending、attempts+1、last_error 记下原因、
//      下次执行时间按退避策略排期 ⇒ 「失败」既不静默也不假装成功。
//   ② 失败轮**不删对象**：存储不可用时任务**不得**进 done（否则对象还在盘上、
//      账本却说删了 ⇒ 泄漏且不可见）。
//   ③ 失败在诊断面**可见**：diagnosticsGCJobs 一屏能看到积压（pending>0），
//      而不是「一切正常」（core#102 同律：失败必须可见）。
//   ④ **下一轮自动恢复**：存储恢复后（同一 worker、同一任务）任务转 done、
//      错误清空、对象真被删 ⇒ 不需要人工干预、也不需要重新入队。
//   ⑤ **同一任务不重复执行**：done 之后不再被认领（对象只删一次）；
//      并发 worker 也不能重复执行（租约 CAS 生效 ⇒ 第二个 worker 本轮拿不到）。
//   ⑥ 认领阶段 DB 故障 ⇒ RunOnce **如实报错**、任务状态不被改坏。
//
// 怎么让它变红（负对照，详见交付说明的实跑记录）：
//   - 把 execute 里的 delete 失败分支改成「直接 MarkDone」⇒ ②红
//     （对象没删却记成已删 ⇒ 泄漏且不可见）；
//   - 把 fail() 改成「吞掉错误不落库」⇒ ①③红（失败不可见、无退避）；
//   - 把 ClaimDue 的 CAS 去掉 ⇒ ⑤的并发判据红（两个 worker 同时执行同一任务）。
//
// 为什么要「手动打一轮」而不是等时钟：后台任务的启动口 startBGLoop 有预算地板
// （gate.CheckTaskBudget：周期 ≥ 1 分钟、首跑延迟 ≤ 周期），等不起也不该等；
// 任务本体函数 gc.RunOnce(ctx) 可以直接手动调用（与 bootstrap/tasks.go 的
// worker 循环体同一入口）。

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	adminApp "github.com/pigeonbox/core/app/admin"
	"github.com/pigeonbox/core/repo/db"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
)

// gcFaultDeleter 可注入失败的对象删除能力（GC 执行器最窄接口）。
type gcFaultDeleter struct {
	err   error
	calls []string
}

func (d *gcFaultDeleter) DeleteFile(_ context.Context, relPath string) error {
	d.calls = append(d.calls, relPath)
	if d.err != nil {
		return d.err
	}
	return nil
}

// callCount 已尝试删除的次数（判「同一任务不重复执行」）。
func (d *gcFaultDeleter) callCount() int { return len(d.calls) }

// newGCFaultDB 起 GC worker 的测试库（**文件型** SQLite：便于「关库」注入
// DB 故障后重新打开核对任务状态没被改坏）。
func newGCFaultDB(t *testing.T) (*gorm.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gc.db")
	g, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.GCJob{}, &model.FileCode{}, &model.FileCodeFile{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	// 注册为后台任务句柄（tasksDB() 的唯一访问点）⇒ 诊断面能读到同一库。
	db.SetDatabaseInstance(g)
	t.Cleanup(func() { db.SetDatabaseInstance(nil) })
	return g, path
}

// newGCFaultWorker 构造一个 GC worker（执行器）：可控时钟 + 可注入失败的删除器。
//
// 退避缩到秒级（生产是分钟级）⇒ 故障恢复的「下一轮」可测。
func newGCFaultWorker(g *gorm.DB, deleter *gcFaultDeleter, now func() time.Time) *adminApp.GCService {
	return adminApp.NewGCService(g, deleter,
		adminApp.WithGCClock(now),
		adminApp.WithGCBackoff(dao.GCBackoffPolicy{Base: time.Second, Cap: time.Minute, MaxAttempts: 5}),
		adminApp.WithGCLease(time.Minute),
	)
}

// enqueueGCJob 入队一条待删任务（与 purge outbox 登记同形态）。
func enqueueGCJob(t *testing.T, g *gorm.DB, objectKey string, due time.Time) model.GCJob {
	t.Helper()
	job := model.GCJob{
		ObjectKey:     objectKey,
		Reason:        model.GCReasonPurge,
		State:         model.GCStatePending,
		NextAttemptAt: due,
	}
	require.NoError(t, g.Create(&job).Error)
	return job
}

// TestGCJobsWorker_StorageUnavailable_FailsThenRecovers 主用例：
// 存储不可用 ⇒ 失败被记录（不删对象、任务不进 done、诊断可见）⇒
// 存储恢复 ⇒ 下一轮自动完成 ⇒ 同一任务不再重复执行。
func TestGCJobsWorker_StorageUnavailable_FailsThenRecovers(t *testing.T) {
	g, _ := newGCFaultDB(t)
	now := time.Now()
	clock := now
	deleter := &gcFaultDeleter{}
	gc := newGCFaultWorker(g, deleter, func() time.Time { return clock })
	ctx := context.Background()

	const objectKey = "uploads/2026/11/18/gc-target.bin"
	job := enqueueGCJob(t, g, objectKey, now)

	// 注入存储故障。
	deleter.err = errors.New("storage unavailable: 503 Service Unavailable")

	// ① 第一轮：失败被记录。
	n, err := gc.RunOnce(ctx)
	require.NoError(t, err, "单任务失败不应让整轮报错（失败要记在任务上）")
	assert.Equal(t, 1, n, "本轮应认领并处理 1 条任务")

	var after model.GCJob
	require.NoError(t, g.First(&after, job.ID).Error)
	assert.Equal(t, model.GCStatePending, after.State, "失败任务必须回到 pending（可重试），不得进 done")
	assert.EqualValues(t, 1, after.Attempts, "失败必须计一次尝试")
	assert.Contains(t, after.LastError, "storage unavailable", "失败原因必须落库（可诊断）")
	assert.True(t, after.NextAttemptAt.After(now), "失败后必须按退避策略排期（不得立即热重试）")

	// ② 失败轮不删对象、任务不进 done。
	assert.Equal(t, 1, deleter.callCount(), "失败轮应尝试过一次删除")
	assert.NotEqual(t, model.GCStateDone, after.State, "对象没删成不得记成已完成")

	// ③ 诊断面可见（失败不静默）。
	diag := diagnosticsGCJobs(ctx)
	assert.EqualValues(t, 1, diag.Pending, "积压必须在诊断面可见")
	assert.Zero(t, diag.Dead, "未耗尽尝试次数不得进死信")
	assert.Empty(t, diag.Error, "采集本身不应报错")

	// 退避期内（时钟没到）⇒ 本轮不该再动它（热重试防护）。
	n, err = gc.RunOnce(ctx)
	require.NoError(t, err)
	assert.Zero(t, n, "退避期内不得重复执行同一任务")
	assert.Equal(t, 1, deleter.callCount(), "退避期内不得重复尝试删除")

	// ④ 存储恢复 + 时钟推进过退避期 ⇒ 下一轮自动完成。
	deleter.err = nil
	clock = clock.Add(2 * time.Second)
	n, err = gc.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "恢复后下一轮应处理那条任务")

	require.NoError(t, g.First(&after, job.ID).Error)
	assert.Equal(t, model.GCStateDone, after.State, "恢复后任务必须转 done")
	assert.Empty(t, after.LastError, "成功后失败原因必须清空")
	assert.Equal(t, 2, deleter.callCount(), "恢复后应真的删了一次对象")

	// ⑤ 同一任务不重复执行：再跑一轮什么都不做，对象不被删第二次。
	n, err = gc.RunOnce(ctx)
	require.NoError(t, err)
	assert.Zero(t, n, "已 done 的任务不得被重复认领")
	assert.Equal(t, 2, deleter.callCount(), "同一任务不得重复删除对象")
}

// TestGCJobsWorker_ConcurrentWorkerDoesNotDuplicate 租约 CAS：
// 任务被一个 worker 认领（running + 租约）后，第二个 worker 本轮**拿不到**
// ⇒ 同一任务不会被并发执行两次。
func TestGCJobsWorker_ConcurrentWorkerDoesNotDuplicate(t *testing.T) {
	g, _ := newGCFaultDB(t)
	now := time.Now()
	clock := now
	deleterA := &gcFaultDeleter{}
	deleterB := &gcFaultDeleter{}
	workerA := newGCFaultWorker(g, deleterA, func() time.Time { return clock })
	workerB := newGCFaultWorker(g, deleterB, func() time.Time { return clock })
	ctx := context.Background()

	job := enqueueGCJob(t, g, "uploads/2026/11/18/gc-cas.bin", now)

	// worker A 认领（只认领不执行：直接用 repo 的 ClaimDue，与 worker 同一路径）。
	repo := dao.NewGCJobRepository(g)
	claimed, err := repo.ClaimDue(ctx, now, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, claimed, "应有任务可认领")
	assert.Equal(t, job.ID, claimed.ID)

	// worker B 本轮拿不到（租约未过期）⇒ 同一任务不会被并发执行两次。
	n, err := workerB.RunOnce(ctx)
	require.NoError(t, err)
	assert.Zero(t, n, "任务已被别的 worker 认领（租约未过期）⇒ 本轮不得再执行")
	assert.Zero(t, deleterB.callCount(), "并发 worker 不得执行同一任务")

	// 持租约的 worker A 完成它（对象删一次）。
	require.NoError(t, repo.MarkDone(ctx, claimed.ID))
	n, err = workerA.RunOnce(ctx)
	require.NoError(t, err)
	assert.Zero(t, n, "已 done 的任务不得被重复执行")
	assert.Zero(t, deleterA.callCount(), "同一任务不得重复删除对象")
}

// TestGCJobsWorker_DBBroken_ReportsErrorAndKeepsTaskState 认领阶段 DB 故障
// ⇒ RunOnce **如实报错**（不静默），且任务状态不被改坏。
func TestGCJobsWorker_DBBroken_ReportsErrorAndKeepsTaskState(t *testing.T) {
	g, dbPath := newGCFaultDB(t)
	now := time.Now()
	clock := now
	deleter := &gcFaultDeleter{}
	gc := newGCFaultWorker(g, deleter, func() time.Time { return clock })
	ctx := context.Background()

	job := enqueueGCJob(t, g, "uploads/2026/11/18/gc-dbbroken.bin", now)

	sqlDB, err := g.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close()) // 关库 ⇒ 认领必失败

	n, err := gc.RunOnce(ctx)
	require.Error(t, err, "认领失败必须如实报错（不得静默吞掉）")
	assert.Zero(t, n, "认领失败时不得报告处理条数")
	assert.Zero(t, deleter.callCount(), "认领失败时不得触碰对象")

	// 重新打开库核对：任务仍是 pending（没被误标成 done/skipped/dead）。
	reopened, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	require.NoError(t, err)
	var after model.GCJob
	require.NoError(t, reopened.First(&after, job.ID).Error)
	assert.Equal(t, model.GCStatePending, after.State, "认领失败不得改坏任务状态")
	assert.Zero(t, after.Attempts, "认领失败不得凭空计尝试次数")
}
