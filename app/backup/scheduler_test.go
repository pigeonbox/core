package backup

// 调度器单测：一次完整备份（导出→落盘→改名→保留），以及三条硬约束的
// 负对照（失败不删旧包 / 不留半个包 / 并发拒绝）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/repo/db/model"
)

// failingLister 列举对象就失败（模拟存储后端故障 ⇒ 导出整体失败）。
type failingLister struct{}

func (failingLister) ListReferencedObjectPaths(context.Context) ([]string, error) {
	return nil, fmt.Errorf("后端不可用")
}

// newTestScheduler 建一个能真导出（文件型 SQLite + 内存对象存储）的调度器。
func newTestScheduler(t *testing.T, keep int, now time.Time) (*Scheduler, string, *fakeStore) {
	t.Helper()
	dbDir := t.TempDir()
	db := newTestDB(t, dbDir)
	seedFileCodes(t, db,
		model.FileCode{Code: "ABCD12", FilePath: "uploads/2026/11/06/a.txt", Size: 5, Status: "normal"},
	)
	store := newFakeStore()
	store.data["uploads/2026/11/06/a.txt"] = []byte("hello")
	svc := NewService(db, fakeLister{paths: []string{"uploads/2026/11/06/a.txt"}}, store)

	backupDir := filepath.Join(t.TempDir(), "backups")
	sch, err := NewScheduler(svc, SchedulerConfig{Dir: backupDir, Keep: keep, At: "03:00"})
	require.NoError(t, err)
	sch.now = func() time.Time { return now }
	return sch, backupDir, store
}

// seedPkg 往目录里放一个「名字合法、内容是垃圾」的旧包（保留策略只看名字）。
func seedPkg(t *testing.T, dir string, ts time.Time) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, FileName(ts))
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))
	return path
}

// TestScheduler_RunOnce 一次备份：包落地、无临时残骸、manifest 计数进结果、
// 包内含 DB 快照（能拿它恢复 ⇒ 不是空壳）。
func TestScheduler_RunOnce(t *testing.T) {
	now := time.Date(2026, 11, 6, 3, 0, 0, 0, time.Local)
	sch, dir, _ := newTestScheduler(t, 7, now)

	res, err := sch.RunOnce(context.Background())
	require.NoError(t, err)
	require.NotNil(t, res)

	assert.Equal(t, FileName(now), res.Name)
	assert.Equal(t, filepath.Join(dir, FileName(now)), res.Path)
	assert.Greater(t, res.Size, int64(0))
	assert.Equal(t, int64(1), res.ObjectCount)
	assert.Equal(t, int64(5), res.ObjectBytes)
	assert.Equal(t, int64(1), res.FileCount)
	assert.Zero(t, res.MissingCount)
	assert.Empty(t, res.Pruned)
	assert.Empty(t, res.PruneErrors)

	// 成型包在，临时文件不在。
	_, statErr := os.Stat(res.Path)
	require.NoError(t, statErr)
	_, partialErr := os.Stat(res.Path + PartialSuffix)
	assert.True(t, os.IsNotExist(partialErr), "成功后不得残留 .partial")

	// 包里必须有 DB 快照（否则是个不能恢复的空壳）。
}

// TestScheduler_IdempotentPerDay 一天一份：跑过之后当天不再到期，跨天恢复到期。
func TestScheduler_IdempotentPerDay(t *testing.T) {
	now := time.Date(2026, 11, 6, 3, 0, 0, 0, time.Local)
	sch, _, _ := newTestScheduler(t, 7, now)

	due, err := sch.Due()
	require.NoError(t, err)
	assert.True(t, due, "到点且当天没包 ⇒ 该备份")

	_, err = sch.RunOnce(context.Background())
	require.NoError(t, err)

	// 负对照：同一天再判一次 ⇒ 不再到期（否则每 10 分钟循环都会重导一遍）。
	due, err = sch.Due()
	require.NoError(t, err)
	assert.False(t, due, "当天已有包 ⇒ 不得重复备份")

	// 跨天：次日 03:00 又到期。
	sch.now = func() time.Time { return now.Add(24 * time.Hour) }
	due, err = sch.Due()
	require.NoError(t, err)
	assert.True(t, due, "次日到点应再次备份")

	// RunIfDue 未到期时返回 (nil, nil) —— 循环据此只打日志不干活。
	sch.now = func() time.Time { return now.Add(time.Hour) }
	res, err := sch.RunIfDue(context.Background())
	require.NoError(t, err)
	assert.Nil(t, res)
}

// TestScheduler_Retention 保留策略：成功后只留最新 keep 份，且最新的那份必留。
func TestScheduler_Retention(t *testing.T) {
	now := time.Date(2026, 11, 6, 3, 0, 0, 0, time.Local)
	sch, dir, _ := newTestScheduler(t, 2, now)

	day := func(d int) time.Time { return time.Date(2026, 11, d, 3, 0, 0, 0, time.Local) }
	oldest := seedPkg(t, dir, day(1))
	middle := seedPkg(t, dir, day(3))
	newestOld := seedPkg(t, dir, day(5))

	res, err := sch.RunOnce(context.Background())
	require.NoError(t, err)
	require.NotNil(t, res)

	// 共 4 份，keep=2 ⇒ 删最旧的两份（1 号、3 号）。
	require.Len(t, res.Pruned, 2)
	assert.Equal(t, filepath.Base(middle), res.Pruned[0])
	assert.Equal(t, filepath.Base(oldest), res.Pruned[1])
	for _, path := range []string{oldest, middle} {
		_, statErr := os.Stat(path)
		assert.True(t, os.IsNotExist(statErr), "应被保留策略删除: %s", path)
	}
	// 最新的一份旧包与本次新包都在。
	for _, path := range []string{newestOld, res.Path} {
		_, statErr := os.Stat(path)
		assert.NoError(t, statErr)
	}
}

// TestScheduler_RetentionDisabled keep<=0 = 不限 ⇒ 一份都不删。
func TestScheduler_RetentionDisabled(t *testing.T) {
	now := time.Date(2026, 11, 6, 3, 0, 0, 0, time.Local)
	sch, dir, _ := newTestScheduler(t, 0, now)
	seedPkg(t, dir, time.Date(2026, 10, 1, 3, 0, 0, 0, time.Local))

	res, err := sch.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Empty(t, res.Pruned, "keep<=0 时不得删除任何旧包")

	files, err := ListFiles(dir)
	require.NoError(t, err)
	assert.Len(t, files, 2)
}

// TestScheduler_FailureKeepsOldBackups 负对照：导出失败 ⇒ 报错、**旧包一份不少**、
// 不留半点临时文件。
//
// 这条是整套机制的安全底线：失败之后删旧包，等于「新备份没成、旧备份也没了」。
func TestScheduler_FailureKeepsOldBackups(t *testing.T) {
	now := time.Date(2026, 11, 6, 3, 0, 0, 0, time.Local)
	db := newTestDB(t, t.TempDir())
	svc := NewService(db, failingLister{}, newFakeStore())
	dir := filepath.Join(t.TempDir(), "backups")
	sch, err := NewScheduler(svc, SchedulerConfig{Dir: dir, Keep: 1, At: "03:00"})
	require.NoError(t, err)
	sch.now = func() time.Time { return now }

	old := seedPkg(t, dir, time.Date(2026, 11, 4, 3, 0, 0, 0, time.Local))
	older := seedPkg(t, dir, time.Date(2026, 11, 3, 3, 0, 0, 0, time.Local))

	res, err := sch.RunOnce(context.Background())
	require.Error(t, err)
	assert.Nil(t, res)

	// 旧包全在（keep=1 本会删掉它们，但失败路径不得清理）。
	for _, path := range []string{old, older} {
		_, statErr := os.Stat(path)
		assert.NoError(t, statErr, "失败时不得删除旧备份: %s", path)
	}
	// 临时残骸必须被清掉（否则会占空间、且看起来像半个包）。
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), PartialSuffix, "失败后不得残留临时文件")
	}
	// 状态里能看见失败原因。
	st := sch.Status()
	assert.Contains(t, st.LastError, "导出备份失败")
	assert.False(t, st.Running, "失败后必须解除 running")
}

// TestScheduler_ErrBusy 并发导出必须被拒绝（定时任务与「立即备份」会同时到）。
func TestScheduler_ErrBusy(t *testing.T) {
	now := time.Date(2026, 11, 6, 3, 0, 0, 0, time.Local)
	sch, _, _ := newTestScheduler(t, 7, now)

	sch.runMu.Lock() // 模拟已有任务在跑
	res, err := sch.RunOnce(context.Background())
	assert.ErrorIs(t, err, ErrBusy)
	assert.Nil(t, res)
	sch.runMu.Unlock()

	// 释放后可用。
	res, err = sch.RunOnce(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, res)
}

// TestNewScheduler_Validation 目录与时刻的校验：拒绝相对路径（部署形态一变就写错
// 地方）与畸形时刻。
func TestNewScheduler_Validation(t *testing.T) {
	db := newTestDB(t, t.TempDir())
	svc := NewService(db, fakeLister{}, newFakeStore())

	_, err := NewScheduler(nil, SchedulerConfig{Dir: "/tmp/x"})
	assert.Error(t, err)

	for _, bad := range []string{"", ".", "backups", "./data/backups"} {
		_, err := NewScheduler(svc, SchedulerConfig{Dir: bad})
		assert.Error(t, err, "相对/空目录必须拒绝: %q", bad)
	}

	_, err = NewScheduler(svc, SchedulerConfig{Dir: t.TempDir(), At: "25:00"})
	assert.Error(t, err)

	_, err = NewScheduler(svc, SchedulerConfig{Dir: t.TempDir(), At: ""})
	assert.NoError(t, err, "空串取默认 03:00")
}

// TestScheduler_Status 状态快照不阻塞、字段与配置一致。
func TestScheduler_Status(t *testing.T) {
	now := time.Date(2026, 11, 6, 3, 0, 0, 0, time.Local)
	sch, dir, _ := newTestScheduler(t, 3, now)

	st := sch.Status()
	assert.Equal(t, dir, st.Dir)
	assert.Equal(t, 3, st.Keep)
	assert.Equal(t, "03:00", st.At)
	assert.False(t, st.Running)
	assert.Empty(t, st.LastError)

	_, err := sch.RunOnce(context.Background())
	require.NoError(t, err)

	st = sch.Status()
	assert.Equal(t, FileName(now), st.LastFile)
	assert.Greater(t, st.LastSize, int64(0))
	assert.Equal(t, now.Unix(), st.LastRunAt.Unix())
}

// TestScheduler_PackageIsRestorable 端到端：定时产出的包**真的能恢复**。
//
// 为什么必须有这一条：在线导出最容易「看着成功、实际半截」——包生成了、大小合理、
// 但里面没有 DB 快照或对象对不上。只断言「文件存在」等于没断言。
// 这里把包喂给同一个 Restore 实现，恢复到一个**全新目录**（等价换机器），
// 再逐项核对：取件码逐字一致、对象内容逐字一致。
func TestScheduler_PackageIsRestorable(t *testing.T) {
	now := time.Date(2026, 11, 6, 3, 0, 0, 0, time.Local)
	sch, _, _ := newTestScheduler(t, 7, now)

	res, err := sch.RunOnce(context.Background())
	require.NoError(t, err)

	pkg, err := os.Open(res.Path)
	require.NoError(t, err)
	defer func() { _ = pkg.Close() }()

	// 恢复目标：全新目录 + 全新对象存储（= 换了一台机器）。
	targetDir := t.TempDir()
	targetDBPath := filepath.Join(targetDir, "restored.db")
	targetDB, err := gorm.Open(sqlite.Open(targetDBPath), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := targetDB.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close()) // 恢复要替换这个文件 ⇒ 先释放句柄

	targetStore := newFakeStore()
	restoreSvc := NewService(targetDB, fakeLister{}, targetStore)
	result, err := restoreSvc.Restore(context.Background(), pkg, RestoreOptions{
		Confirm: true,
		DBPath:  targetDBPath,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.DBRestored)
	assert.Equal(t, int64(1), result.ObjectsRestored)
	assert.Empty(t, result.MissingObjects)

	// 对象内容逐字一致。
	assert.Equal(t, []byte("hello"), targetStore.data["uploads/2026/11/06/a.txt"])

	// DB 里的取件码逐字一致（「分享链接不变」这条承诺的实证）。
	restored, err := gorm.Open(sqlite.Open(targetDBPath), &gorm.Config{})
	require.NoError(t, err)
	var rows []model.FileCode
	require.NoError(t, restored.Find(&rows).Error)
	require.Len(t, rows, 1)
	assert.Equal(t, "ABCD12", rows[0].Code)
	assert.Equal(t, "uploads/2026/11/06/a.txt", rows[0].FilePath)
}
