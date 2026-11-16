package admin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// newBlobGCTestEnv janitor 测试环境 + blobs/file_references 两表
// （newJanitorTestEnv 的 AutoMigrate 清单不含新表，本文件自行补建，
// 不动 janitor_test.go——#196 在途改动面）。
func newBlobGCTestEnv(t *testing.T) (string, *Janitor, *gorm.DB) {
	t.Helper()
	root, svc, g := newJanitorTestEnv(t)
	require.NoError(t, g.AutoMigrate(&model.Blob{}, &model.FileReference{}))
	return root, NewJanitor(g, svc, 90), g
}

// writeObj 在存储根下写一个物理对象，返回相对路径。
func writeObj(t *testing.T, root, rel string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
	require.NoError(t, os.WriteFile(abs, []byte("payload"), 0o644))
}

// seedBlob 播 blob 行（state/trashed_at 可控）。
func seedBlob(t *testing.T, g *gorm.DB, key, state string, trashedAt *time.Time) model.Blob {
	t.Helper()
	b := model.Blob{ObjectKey: key, State: state, TrashedAt: trashedAt}
	require.NoError(t, g.Create(&b).Error)
	return b
}

// 正例：归零 + 过宽限 + 无路径引用 ⇒ 删对象 + 账本 purged。
func TestReclaimBlobs_ZeroRefPastGraceReclaims(t *testing.T) {
	root, j, g := newBlobGCTestEnv(t)
	ctx := context.Background()
	const rel = "uploads/2026/11/16/reclaim-me.bin"
	writeObj(t, root, rel)
	old := time.Now().Add(-48 * time.Hour)
	b := seedBlob(t, g, rel, model.BlobTrashed, &old)

	n, err := j.ReclaimBlobs(ctx, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "归零过宽限应回收 1 个")

	assert.NoFileExists(t, filepath.Join(root, filepath.FromSlash(rel)), "物理对象应已删除")
	var got model.Blob
	require.NoError(t, g.First(&got, b.ID).Error)
	assert.Equal(t, model.BlobPurged, got.State, "账本应推进到 purged 终态")
	require.NotNil(t, got.PurgedAt)
}

// 负对照①（issue 验收）：引用未归零不回收——账本仍有 active 引用。
func TestReclaimBlobs_ActiveRefBlocksReclaim(t *testing.T) {
	root, j, g := newBlobGCTestEnv(t)
	ctx := context.Background()
	const rel = "uploads/2026/11/16/still-referenced.bin"
	writeObj(t, root, rel)
	old := time.Now().Add(-48 * time.Hour)
	b := seedBlob(t, g, rel, model.BlobTrashed, &old)
	// 仍有存活引用（共享对象另一分享未删）;
	require.NoError(t, g.Create(&model.FileReference{
		BlobID: b.ID, FileCodeID: 42, State: model.RefActive,
	}).Error)

	n, err := j.ReclaimBlobs(ctx, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "负对照①：引用未归零不得回收")
	assert.FileExists(t, filepath.Join(root, filepath.FromSlash(rel)), "对象必须保留")
	var got model.Blob
	require.NoError(t, g.First(&got, b.ID).Error)
	assert.Equal(t, model.BlobTrashed, got.State, "状态不得推进")
}

// 负对照②（issue 验收）：软删不回收——路径仍被软删分享行引用（账本口径
// 没有引用行也一样挡住：存量分享无账本行，靠路径守卫）。
func TestReclaimBlobs_SoftDeletedShareBlocksReclaim(t *testing.T) {
	root, j, g := newBlobGCTestEnv(t)
	ctx := context.Background()
	const rel = "uploads/2026/11/16/soft-deleted-owner.bin"
	writeObj(t, root, rel)
	old := time.Now().Add(-48 * time.Hour)
	b := seedBlob(t, g, rel, model.BlobTrashed, &old)
	// 软删分享行引用该路径（回收站行仍占对象）——**无账本引用行**，;
	// 模拟存量分享（首片不回填）与软删双重形态。;
	fc := &model.FileCode{Code: "SOFTBLOB", FilePath: rel, Text: "x"}
	require.NoError(t, g.Create(fc).Error)
	require.NoError(t, g.Delete(&model.FileCode{}, "code = ?", "SOFTBLOB").Error)

	n, err := j.ReclaimBlobs(ctx, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "负对照②：软删（含存量无账本行）不得回收")
	assert.FileExists(t, filepath.Join(root, filepath.FromSlash(rel)), "对象必须保留")
	var got model.Blob
	require.NoError(t, g.First(&got, b.ID).Error)
	assert.Equal(t, model.BlobTrashed, got.State, "状态不得推进")
}

// 宽限未到：trashed_at 太新 ⇒ 不进候选（负对照：未过宽限不回收）。
func TestReclaimBlobs_GraceNotElapsedSkips(t *testing.T) {
	root, j, g := newBlobGCTestEnv(t)
	ctx := context.Background()
	const rel = "uploads/2026/11/16/fresh-trash.bin"
	writeObj(t, root, rel)
	fresh := time.Now()
	b := seedBlob(t, g, rel, model.BlobTrashed, &fresh)

	n, err := j.ReclaimBlobs(ctx, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "宽限未到不得回收")
	assert.FileExists(t, filepath.Join(root, filepath.FromSlash(rel)))
	var got model.Blob
	require.NoError(t, g.First(&got, b.ID).Error)
	assert.Equal(t, model.BlobTrashed, got.State)
}

// 幂等收敛：对象已被既有同步删清掉（文件不存在）⇒ 直接推进 purged，
// 不因 DeleteFile 报错卡在 purge。
func TestReclaimBlobs_MissingObjectConvergesToPurged(t *testing.T) {
	_, j, g := newBlobGCTestEnv(t)
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour)
	b := seedBlob(t, g, "uploads/2026/11/16/already-gone.bin", model.BlobTrashed, &old)

	n, err := j.ReclaimBlobs(ctx, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	var got model.Blob
	require.NoError(t, g.First(&got, b.ID).Error)
	assert.Equal(t, model.BlobPurged, got.State, "对象已不存在应幂等收敛到 purged")
}

// 存活分享（未删）在候选之外：active 态根本不进 ListReclaimable。
func TestReclaimBlobs_ActiveStateNotCandidate(t *testing.T) {
	root, j, g := newBlobGCTestEnv(t)
	ctx := context.Background()
	const rel = "uploads/2026/11/16/live-share.bin"
	writeObj(t, root, rel)
	seedBlob(t, g, rel, model.BlobActive, nil)

	n, err := j.ReclaimBlobs(ctx, 0)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "active 态不是回收候选")
	assert.FileExists(t, filepath.Join(root, filepath.FromSlash(rel)))
}

// 观测接线（core#172 第二片）：ReclaimBlobs 每轮**回收后**刷新状态分布
// 与回收积压。负对照：过宽限的推进 purged、宽限期内的留 trashed 且计入
// 积压、active 不动。
func TestReclaimBlobs_UpdatesMetrics(t *testing.T) {
	_, j, g := newBlobGCTestEnv(t)
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour)
	recent := time.Now().Add(-1 * time.Hour)
	seedBlob(t, g, "uploads/2026/11/16/m1.bin", model.BlobTrashed, &old)    // 过宽限 ⇒ purged
	seedBlob(t, g, "uploads/2026/11/16/m3.bin", model.BlobTrashed, &recent) // 宽限期内 ⇒ 留 trashed
	seedBlob(t, g, "uploads/2026/11/16/m2.bin", model.BlobActive, nil)

	n, err := j.ReclaimBlobs(ctx, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "只有过宽限的那条被回收")

	assert.Equal(t, 1.0, gatherGauge(t, "pb_blob_reclaim_backlog", nil),
		"积压 = 流水线未收敛行（宽限期内的 trashed 仍算积压）")
	assert.Equal(t, 1.0, gatherGauge(t, "pb_blob_states", map[string]string{"state": "trashed"}),
		"宽限期内的 trashed 必须保留在分布里")
	assert.Equal(t, 1.0, gatherGauge(t, "pb_blob_states", map[string]string{"state": "purged"}),
		"过宽限的应推进到 purged")
	assert.Equal(t, 1.0, gatherGauge(t, "pb_blob_states", map[string]string{"state": "active"}),
		"active 不受影响")
}

// gatherGauge 从默认注册表读 gauge 值（跨包不导出变量，走 Gather 接口）。
func gatherGauge(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			match := true
			for k, v := range labels {
				found := false
				for _, lp := range m.GetLabel() {
					if lp.GetName() == k && lp.GetValue() == v {
						found = true
					}
				}
				if !found {
					match = false
				}
			}
			if match && m.GetGauge() != nil {
				return m.GetGauge().GetValue()
			}
		}
	}
	t.Fatalf("metric %s not found", name)
	return 0
}
