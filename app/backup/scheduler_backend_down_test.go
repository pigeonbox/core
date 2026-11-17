package backup

// scheduler_backend_down_test.go —— 定时备份遇到 **存储后端不可达** 时的行为。
//
// 背景：后端宕机时导出**必须整体失败**（不得产出「看着成功、实际缺对象」的包）⇒
// 调度器这一层必须守住既有三条硬约束：
//   ① 失败整体报错、结果为 nil（不是「半个成功」）；
//   ② 失败时旧包一个都不删（keep=1 本会删掉它们）——失败后删旧包＝灾备放大器；
//   ③ 不产生「看起来成功」的包，也不残留 .partial 临时文件；
//   ④ 失败在状态面可见（LastError 非空、LastFile 为空）。
//
// 负对照：把导出侧的「后端故障 ⇒ 整体失败」改回吞错（记成 missing 继续导出）
// ⇒ ①③④红（导出「成功」⇒ 留下成型包 + 旧包被按保留策略删掉）。

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/model"
)

// TestScheduler_BackendDown_NoPackageNoPrune 后端不可达 ⇒ 定时备份整体报错、
// 旧包一个不删、不留成型包、不残留 .partial、失败在状态面可见。
func TestScheduler_BackendDown_NoPackageNoPrune(t *testing.T) {
	now := time.Date(2026, 11, 18, 3, 0, 0, 0, time.Local)
	db := newTestDB(t, t.TempDir())
	seedFileCodes(t, db,
		model.FileCode{Code: "ABCD12", FilePath: "uploads/2026/11/18/a.txt", Size: 5, Status: "normal"},
	)
	// 后端不可达（装配方未转发探测能力时走的是兜底路径——形态同生产）。
	store := &probeLessStore{data: map[string][]byte{}, down: true}
	svc := NewService(db, fakeLister{paths: []string{"uploads/2026/11/18/a.txt"}}, store)

	dir := filepath.Join(t.TempDir(), "backups")
	sch, err := NewScheduler(svc, SchedulerConfig{Dir: dir, Keep: 1, At: "03:00"})
	require.NoError(t, err)
	sch.now = func() time.Time { return now }

	// 一份旧包（keep=1 ⇒ 成功路径本会删掉它）。
	old := seedPkg(t, dir, time.Date(2026, 11, 17, 3, 0, 0, 0, time.Local))

	// ① 失败整体报错。
	res, err := sch.RunOnce(context.Background())
	require.Error(t, err, "存储后端不可达 ⇒ 定时备份必须整体报错")
	assert.Nil(t, res, "失败不得返回一个「看起来成功」的结果对象")

	// ② 失败时旧包一个都不删。
	_, statErr := os.Stat(old)
	assert.NoError(t, statErr, "失败时不得删除旧备份（灾备放大器）")

	// ③ 不产生成型包、也不残留 .partial。
	_, statErr = os.Stat(filepath.Join(dir, FileName(now)))
	assert.True(t, os.IsNotExist(statErr), "失败不得留下成型包（会被当成今天已备份）")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), PartialSuffix, "失败后不得残留临时文件（半个包）")
	}

	// ④ 失败在状态面可见、且没被记成成功包。
	st := sch.Status()
	assert.NotEmpty(t, st.LastError, "失败原因必须在状态面可见")
	assert.Empty(t, st.LastFile, "失败不得把任何包记成成功包")
	assert.False(t, st.Running, "失败后必须解除 running")

	// 失败后仍然到期重试（今天没有成型包 ⇒ 下一轮补跑）。
	due, err := sch.Due()
	require.NoError(t, err)
	assert.True(t, due, "失败后今天仍算「没备份」⇒ 下一轮必须补跑")
}
