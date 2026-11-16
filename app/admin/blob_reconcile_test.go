package admin

import (
	"context"
	"testing"
	"time"

	"github.com/pigeonbox/core/pkg/metrics"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assertDriftCounts 断言四类漂移计数与对应 gauge 标签值（want 缺席键=0）。
// counts 与 gauge 一起断言：Drift* 常量 ↔ SetBlobReconcileDrift 标签清单
// 两侧同源，任何一侧漂移即红（pkg/metrics/blob.go 注释约定）。
func assertDriftCounts(t *testing.T, counts map[string]int, want map[string]int) {
	t.Helper()
	for _, k := range []string{
		DriftDanglingLedger, DriftMissingLedger,
		DriftActiveObjectMissing, DriftPurgedObjectPresent,
	} {
		assert.Equal(t, want[k], counts[k], "drift count[%s]", k)
		assert.Equal(t, float64(want[k]), metrics.BlobReconcileDriftValue(k),
			"gauge pb_blob_reconcile_drift{kind=%s}", k)
	}
}

// 判据①：账本 active 行、路径零引用 ⇒ 悬挂账本行（对象永久泄漏：回收只挑
// trashed/purge，active 零引用永远进不了回收候选）。
func TestScanBlobDrift_DanglingLedger(t *testing.T) {
	root, j, g := newBlobGCTestEnv(t)
	ctx := context.Background()
	const rel = "uploads/2026/11/16/dangling.bin"
	writeObj(t, root, rel)                     // 对象在（否则③也报）
	seedBlob(t, g, rel, model.BlobActive, nil) // active 但全库零路径引用

	counts, err := j.ScanBlobDrift(ctx)
	require.NoError(t, err)
	assertDriftCounts(t, counts, map[string]int{DriftDanglingLedger: 1})
}

// 判据②：路径有引用、账本无行 ⇒ 存量漏账（首片不回填的存量分享）。
// 按去重路径计数：秒传多分享共享同一物理文件只算一笔。
func TestScanBlobDrift_MissingLedger(t *testing.T) {
	_, j, g := newBlobGCTestEnv(t)
	ctx := context.Background()
	const rel = "uploads/2026/11/16/legacy.bin"
	// 两行分享引用同一路径、皆无 blobs 行 → 去重后仍报 1
	require.NoError(t, g.Create(&model.FileCode{Code: "DRIFTML1", FilePath: rel, Text: "x"}).Error)
	require.NoError(t, g.Create(&model.FileCode{Code: "DRIFTML2", FilePath: rel, Text: "x"}).Error)

	counts, err := j.ScanBlobDrift(ctx)
	require.NoError(t, err)
	assertDriftCounts(t, counts, map[string]int{DriftMissingLedger: 1})
}

// 判据③：账本 active 但存储对象不存在 ⇒ 分享指向 404。
// 路径必须有引用（否则①同报，无法证明③独立）。
func TestScanBlobDrift_ActiveObjectMissing(t *testing.T) {
	_, j, g := newBlobGCTestEnv(t)
	ctx := context.Background()
	const rel = "uploads/2026/11/16/vanished.bin"
	seedBlob(t, g, rel, model.BlobActive, nil) // 不写物理对象
	require.NoError(t, g.Create(&model.FileCode{Code: "DRIFTAM1", FilePath: rel, Text: "x"}).Error)

	counts, err := j.ScanBlobDrift(ctx)
	require.NoError(t, err)
	assertDriftCounts(t, counts, map[string]int{DriftActiveObjectMissing: 1})
}

// 判据④：账本 purged（终态说已删）但存储对象仍存在 ⇒ 占盘泄漏。
func TestScanBlobDrift_PurgedObjectPresent(t *testing.T) {
	root, j, g := newBlobGCTestEnv(t)
	ctx := context.Background()
	const rel = "uploads/2026/11/16/zombie.bin"
	writeObj(t, root, rel)
	seedBlob(t, g, rel, model.BlobPurged, nil)

	counts, err := j.ScanBlobDrift(ctx)
	require.NoError(t, err)
	assertDriftCounts(t, counts, map[string]int{DriftPurgedObjectPresent: 1})
}

// 零漂移基线：健康态与非判据状态一律不误报；同时验证 gauge 整表覆盖
// （上一轮残留必须被清零）。
func TestScanBlobDrift_ZeroBaseline(t *testing.T) {
	root, j, g := newBlobGCTestEnv(t)
	ctx := context.Background()

	// 健康 active：路径有引用 + 对象在
	const healthy = "uploads/2026/11/16/healthy.bin"
	writeObj(t, root, healthy)
	seedBlob(t, g, healthy, model.BlobActive, nil)
	require.NoError(t, g.Create(&model.FileCode{Code: "DRIFTZH1", FilePath: healthy, Text: "x"}).Error)

	// 健康 purged：对象已删（终态收敛）
	const purgedOK = "uploads/2026/11/16/purged-ok.bin"
	seedBlob(t, g, purgedOK, model.BlobPurged, nil)

	// 回收流水线中的 trashed：对象在、零路径引用——是待回收态，不是漂移
	const inGrace = "uploads/2026/11/16/in-grace.bin"
	writeObj(t, root, inGrace)
	old := time.Now().Add(-time.Hour)
	seedBlob(t, g, inGrace, model.BlobTrashed, &old)

	// 上传中的 committed：对象在、零引用——尚无分享，不参与判据
	const staged = "uploads/2026/11/16/staged.bin"
	writeObj(t, root, staged)
	seedBlob(t, g, staged, model.BlobCommitted, nil)

	// 先污染 gauge（模拟上一轮残留），扫描后必须整表覆盖清零
	metrics.SetBlobReconcileDrift(map[string]int{
		DriftDanglingLedger: 7, DriftMissingLedger: 7,
		DriftActiveObjectMissing: 7, DriftPurgedObjectPresent: 7,
	})

	counts, err := j.ScanBlobDrift(ctx)
	require.NoError(t, err)
	assertDriftCounts(t, counts, map[string]int{})
}

// 未装配存储时扫描报错且不碰 gauge（宁留上轮读数，不覆盖成假 0）。
func TestScanBlobDrift_StorageNotReady(t *testing.T) {
	_, _, g := newBlobGCTestEnv(t)
	j := NewJanitor(g, nil, 90)
	metrics.SetBlobReconcileDrift(map[string]int{DriftDanglingLedger: 3})

	counts, err := j.ScanBlobDrift(context.Background())
	require.Error(t, err)
	assert.Nil(t, counts)
	assert.Equal(t, 3.0, metrics.BlobReconcileDriftValue(DriftDanglingLedger),
		"扫描失败不得刷新 gauge")
}
