package bootstrap

// core#192 回归：GC 任务面的管理面接线——诊断段（积压/死信一屏）与死信重放
// 端点（POST /admin/storage/gc-jobs/replay 背后的处理函数）。
//
// 守卫点：
//  1. 无任务 ⇒ 各状态 0、无 Error（不误报积压）；
//  2. 各状态种子 ⇒ 计数如实 + 最老 pending 积压时长可见；
//  3. DB 不可用 ⇒ Error 落段（「失败必须可见」，core#102 同律）；
//  4. 重放：dead → pending（立即到期），attempts 归零——重放语义可测；
//  5. 增段不动既有段（与 diagnostics_clamav_test 同纪律）。

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/repo/db"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
)

// newGCJobsDiagDB 内存库 + 注册为后台任务句柄（tasksDB 的测试注入点）。
func newGCJobsDiagDB(t *testing.T) (*gorm.DB, *dao.GCJobRepository) {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.GCJob{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	db.SetDatabaseInstance(g)
	t.Cleanup(func() { db.SetDatabaseInstance(nil) })
	return g, dao.NewGCJobRepository(g)
}

// TestDiagnosticsGCJobs_Empty 无任务 ⇒ 全零、无 Error。
func TestDiagnosticsGCJobs_Empty(t *testing.T) {
	_, _ = newGCJobsDiagDB(t)
	out := diagnosticsGCJobs(context.Background())
	assert.Zero(t, out.Pending)
	assert.Zero(t, out.Dead)
	assert.Empty(t, out.Error, "无任务不得误报错误")
	assert.Zero(t, out.OldestPendingSeconds)
}

// TestDiagnosticsGCJobs_Counts 各状态计数 + 最老 pending 积压时长。
func TestDiagnosticsGCJobs_Counts(t *testing.T) {
	g, _ := newGCJobsDiagDB(t)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, g.Create(&model.GCJob{ObjectKey: "p1", State: model.GCStatePending, NextAttemptAt: now}).Error)
	require.NoError(t, g.Create(&model.GCJob{ObjectKey: "p2", State: model.GCStatePending, NextAttemptAt: now}).Error)
	require.NoError(t, g.Model(&model.GCJob{}).Where("object_key = ?", "p1").
		Update("created_at", now.Add(-90*time.Second)).Error)
	require.NoError(t, g.Create(&model.GCJob{ObjectKey: "r", State: model.GCStateRunning, NextAttemptAt: now}).Error)
	require.NoError(t, g.Create(&model.GCJob{ObjectKey: "d", State: model.GCStateDone, NextAttemptAt: now}).Error)
	require.NoError(t, g.Create(&model.GCJob{ObjectKey: "s", State: model.GCStateSkipped, NextAttemptAt: now}).Error)
	require.NoError(t, g.Create(&model.GCJob{ObjectKey: "x", State: model.GCStateDead, NextAttemptAt: now}).Error)

	out := diagnosticsGCJobs(ctx)
	assert.EqualValues(t, 2, out.Pending)
	assert.EqualValues(t, 1, out.Running)
	assert.EqualValues(t, 1, out.Done)
	assert.EqualValues(t, 1, out.Skipped)
	assert.EqualValues(t, 1, out.Dead, "死信必须一屏可见")
	assert.GreaterOrEqual(t, out.OldestPendingSeconds, int64(90), "最老 pending 积压时长必须可见")
}

// TestDiagnosticsGCJobs_DBNotReady DB 不可用 ⇒ Error 落段（不静默）。
func TestDiagnosticsGCJobs_DBNotReady(t *testing.T) {
	db.SetDatabaseInstance(nil)
	t.Cleanup(func() { db.SetDatabaseInstance(nil) })
	out := diagnosticsGCJobs(context.Background())
	assert.NotEmpty(t, out.Error, "采集失败必须显式落 Error")
}

// TestGCJobsReplayHandler 重放端点：dead → pending（立即到期、attempts 归零）。
func TestGCJobsReplayHandler(t *testing.T) {
	g, _ := newGCJobsDiagDB(t)
	now := time.Now()

	require.NoError(t, g.Create(&model.GCJob{
		ObjectKey: "uploads/gc/replay.bin", State: model.GCStateDead, Attempts: 5,
		NextAttemptAt: now.Add(time.Hour), LastError: "remote 500",
	}).Error)

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.POST("/admin/storage/gc-jobs/replay", gcJobsReplayHandler)
	w := ut.PerformRequest(h.Engine, "POST", "/admin/storage/gc-jobs/replay", nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var job model.GCJob
	require.NoError(t, g.First(&job).Error)
	assert.Equal(t, model.GCStatePending, job.State, "重放必须回到 pending")
	assert.Zero(t, job.Attempts, "重放必须把 attempts 归零")
	assert.Empty(t, job.LastError)
	assert.LessOrEqual(t, job.NextAttemptAt.Unix(), time.Now().Add(time.Minute).Unix(),
		"重放必须立即到期（不等旧退避期）")
}
