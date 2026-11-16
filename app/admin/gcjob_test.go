package admin

// core#192 回归：对象 GC 持久化任务面。
//
// 验收映射（issue）：
//   - 「远端返回 500 三次后，任务仍能续跑并最终完成」⇒ TestGCJob_RetryWithBackoffEventuallyDone
//   - 「两个 share 共享同一 blob，删其一 ⇒ 对象不得被删，任务记为引用中跳过」
//     ⇒ TestGCJob_ReferenceRecheckSkipsSharedBlob
//   - 「杀进程 ⇒ 任务仍能续跑」⇒ outbox 同事务登记（files_purge_test.go）+
//     租约过期接管 ⇒ TestGCJob_LeaseExpiryReclaim
//   - 「dead 有重放命令」⇒ TestGCJob_DeadAndReplay
//   - 「积压可观测」⇒ TestGCJob_MetricsGauge + bootstrap 诊断段

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
)

// gcTestEnv GC 用例环境：注入时钟 + 毫秒级退避（退避期可控推进）。
// 删除桩=mockStorage（failNext 可编程失败；GetStorageStatus 等用例共用）。
type gcTestEnv struct {
	svc     *Service
	gormDB  *gorm.DB
	repo    *dao.GCJobRepository
	gc      *GCService
	deleter *mockStorage
	now     time.Time
}

func newGCTestEnv(t *testing.T) *gcTestEnv {
	t.Helper()
	g := newRestoreTestDB(t)
	svc := NewService(g)
	st := &mockStorage{}
	svc.SetStorage(st)
	env := &gcTestEnv{
		svc:     svc,
		gormDB:  g,
		repo:    dao.NewGCJobRepository(g),
		gc:      svc.GC(),
		deleter: st,
		now:     time.Now(),
	}
	env.gc.nowFn = func() time.Time { return env.now }
	// 毫秒级退避（可测）：attempts=1 ⇒ 20ms, 2 ⇒ 40ms, 3 ⇒ 80ms（封顶 1s）
	env.gc.backoff = dao.GCBackoffPolicy{Base: 20 * time.Millisecond, Cap: time.Second, MaxAttempts: 5}
	env.gc.leaseFor = time.Minute
	return env
}

// TestGCJob_RetryWithBackoffEventuallyDone 远端连报 500 三次后第四次成功：
// 任务经退避续跑并落 done（issue 验收「远端 500 三次后任务仍能续跑并最终完成」）。
func TestGCJob_RetryWithBackoffEventuallyDone(t *testing.T) {
	env := newGCTestEnv(t)
	ctx := context.Background()
	env.deleter.failNext = 3

	require.NoError(t, env.repo.Enqueue(ctx, []*model.GCJob{
		{ObjectKey: "uploads/gc/retry.bin", Reason: model.GCReasonOrphanReconcile, State: model.GCStatePending, NextAttemptAt: env.now},
	}))

	// 第 1 轮：删除失败 ⇒ 退避回 pending
	n, err := env.gc.RunOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	var job model.GCJob
	require.NoError(t, env.gormDB.First(&job).Error)
	require.Equal(t, model.GCStatePending, job.State, "失败后必须退回 pending 排期")
	require.Equal(t, 1, job.Attempts)
	require.Contains(t, job.LastError, "remote 500")

	// 退避期内（20ms）⇒ 不被认领
	processed, err := env.gc.RunOnce(ctx)
	require.NoError(t, err)
	require.Zero(t, processed, "退避期内不得重跑")

	// 推进时钟越过退避期 ×3 轮失败（第 2、3 轮失败，第 4 轮成功）
	for range 3 {
		env.now = env.now.Add(100 * time.Millisecond)
		_, err = env.gc.RunOnce(ctx)
		require.NoError(t, err)
	}
	require.Len(t, env.deleter.deleted, 1, "第四次必须成功")
	require.Equal(t, []string{"uploads/gc/retry.bin"}, env.deleter.deleted)

	var done model.GCJob
	require.NoError(t, env.gormDB.First(&done).Error)
	require.Equal(t, model.GCStateDone, done.State)
}

// TestGCJob_ReferenceRecheckSkipsSharedBlob 两个分享共享同一 blob，删其一：
// purge 登记任务后，worker 的删前引用复核必须拦下删除（对象保留、任务落
// skipped 终态）——「允许延迟，绝不误删」的底线（issue 验收逐字判据）。
func TestGCJob_ReferenceRecheckSkipsSharedBlob(t *testing.T) {
	env := newGCTestEnv(t)
	ctx := context.Background()

	const shared = "uploads/2026/11/16/shared-gc.bin"
	mk := func(code string) *model.FileCode {
		return &model.FileCode{Code: code, FilePath: shared, Text: code, Size: 10, ExpiredCount: -1}
	}
	require.NoError(t, env.svc.fileCodeRepo.Create(ctx, mk("GCSHAR01")))
	require.NoError(t, env.svc.fileCodeRepo.Create(ctx, mk("GCSHAR02")))
	a, err := env.svc.fileCodeRepo.GetByCode(ctx, "GCSHAR01")
	require.NoError(t, err)

	// purge 其一：行硬删 + 任务登记（outbox）
	purged, err := env.svc.PurgeFiles(ctx, []uint{a.ID})
	require.NoError(t, err)
	require.Equal(t, int64(1), purged)

	// worker 消费：引用复核命中（GCSHAR02 仍在）⇒ skipped，对象不删
	processed, err := env.gc.RunOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, processed)
	require.Empty(t, env.deleter.deleted, "共享对象不得被删")

	var job model.GCJob
	require.NoError(t, env.gormDB.First(&job).Error)
	require.Equal(t, model.GCStateSkipped, job.State)
	require.Contains(t, job.LastError, "referenced")

	// 另一个分享随后也被 purge ⇒ 引用归零 ⇒ 下一轮真的删掉
	b, err := env.svc.fileCodeRepo.GetByCode(ctx, "GCSHAR02")
	require.NoError(t, err)
	_, err = env.svc.PurgeFiles(ctx, []uint{b.ID})
	require.NoError(t, err)
	_, err = env.gc.RunOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{shared}, env.deleter.deleted, "最后一个引用消失后对象才被删")
}

// TestGCJob_LeaseExpiryReclaim 租约到期接管：worker A 认领后「崩溃」（不再
// 续租），租约过期后 worker B 必须能重新认领并完成任务——杀进程不丢任务。
func TestGCJob_LeaseExpiryReclaim(t *testing.T) {
	env := newGCTestEnv(t)
	ctx := context.Background()

	require.NoError(t, env.repo.Enqueue(ctx, []*model.GCJob{
		{ObjectKey: "uploads/gc/lease.bin", Reason: model.GCReasonOrphanReconcile, State: model.GCStatePending, NextAttemptAt: env.now},
	}))

	// worker A 认领（拿到租约）但进程被杀 ⇒ 无终态落表
	claimed, err := env.repo.ClaimDue(ctx, env.now, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, claimed, "首认必须成功")

	// 租约期内 worker B 认领 ⇒ 抢不到（CAS 行级互斥）
	stolen, err := env.repo.ClaimDue(ctx, env.now.Add(30*time.Second), time.Minute)
	require.NoError(t, err)
	require.Nil(t, stolen, "租约期内不得被其他 worker 抢占")

	// 租约过期 ⇒ worker B 接管并完成任务
	env.now = env.now.Add(2 * time.Minute)
	processed, err := env.gc.RunOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, processed)
	require.Equal(t, []string{"uploads/gc/lease.bin"}, env.deleter.deleted)

	var job model.GCJob
	require.NoError(t, env.gormDB.First(&job).Error)
	require.Equal(t, model.GCStateDone, job.State)
}

// TestGCJob_DeadAndReplay attempts 耗尽 ⇒ dead 死信；重放（replay 命令语义）
// 后任务回 pending 并在存储恢复后完成。
func TestGCJob_DeadAndReplay(t *testing.T) {
	env := newGCTestEnv(t)
	ctx := context.Background()
	env.deleter.failNext = 100 // 恒失败
	env.gc.backoff = dao.GCBackoffPolicy{Base: 1 * time.Millisecond, Cap: 10 * time.Millisecond, MaxAttempts: 3}

	require.NoError(t, env.repo.Enqueue(ctx, []*model.GCJob{
		{ObjectKey: "uploads/gc/dead.bin", Reason: model.GCReasonPurge, State: model.GCStatePending, NextAttemptAt: env.now},
	}))

	// 3 次失败（每次推进时钟越过退避）⇒ dead
	for range 3 {
		_, err := env.gc.RunOnce(ctx)
		require.NoError(t, err)
		env.now = env.now.Add(50 * time.Millisecond)
	}
	var job model.GCJob
	require.NoError(t, env.gormDB.First(&job).Error)
	require.Equal(t, model.GCStateDead, job.State, "attempts 耗尽必须落死信")
	require.Equal(t, 3, job.Attempts)

	// dead 不再被认领（死信静止，等重放）
	processed, err := env.gc.RunOnce(ctx)
	require.NoError(t, err)
	require.Zero(t, processed)

	// 重放命令语义（POST /admin/storage/gc-jobs/replay 背后的 dao 操作）
	replayed, err := env.repo.RequeueDead(ctx, env.now)
	require.NoError(t, err)
	require.EqualValues(t, 1, replayed)

	// 存储恢复 ⇒ 重放后任务完成
	env.deleter.failNext = 0
	processed, err = env.gc.RunOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, processed)
	require.Equal(t, []string{"uploads/gc/dead.bin"}, env.deleter.deleted)
}

// TestGCJob_CountsByState 每轮末的计数来源（pb_gc_jobs 仪表的数据面）：
// m1 消费完成、m2 退避期未到 ⇒ done=1、pending=1（积压可见——issue 验收
// 「积压与死信可在诊断页看到」的数据基础；仪表刷新见 pkg/metrics gc_test）。
func TestGCJob_CountsByState(t *testing.T) {
	env := newGCTestEnv(t)
	ctx := context.Background()

	require.NoError(t, env.repo.Enqueue(ctx, []*model.GCJob{
		// m1 到期（本轮消费）；m2 退避期未到（本轮跳过 ⇒ pending 积压）
		{ObjectKey: "uploads/gc/m1.bin", State: model.GCStatePending, NextAttemptAt: env.now},
		{ObjectKey: "uploads/gc/m2.bin", State: model.GCStatePending, NextAttemptAt: env.now.Add(time.Hour)},
	}))

	_, err := env.gc.RunOnce(ctx)
	require.NoError(t, err)

	counts, err := env.repo.CountByState(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, counts[model.GCStateDone])
	assert.EqualValues(t, 1, counts[model.GCStatePending])
}

// TestGCJob_JobsForPaths 过滤空路径 + reason/状态注入（purge 入口的形状契约）。
func TestGCJob_JobsForPaths(t *testing.T) {
	env := newGCTestEnv(t)
	jobs := env.gc.JobsForPaths([]string{"a.bin", "", "b.bin"}, model.GCReasonPurge)
	require.Len(t, jobs, 2, "空路径必须被过滤")
	require.Equal(t, "a.bin", jobs[0].ObjectKey)
	require.Equal(t, model.GCStatePending, jobs[0].State)
	require.Equal(t, model.GCReasonPurge, jobs[0].Reason)
}
