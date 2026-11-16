// 守卫②降级的 janitor 行为面（core#172）：
//
//	未覆盖 active 行必须仍被挡（回填失败面安全网）；
//	已覆盖 active 行不再挡（收紧生效——其保护交守卫①）。
package admin

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/model"
)

// 未覆盖 active 行（回填 best-effort 失败面）⇒ 路径守卫必须挡住回收。
func TestReclaimBlobs_UncoveredActiveRowBlocksReclaim(t *testing.T) {
	root, j, g := newBlobGCTestEnv(t)
	ctx := context.Background()
	const rel = "uploads/2026/11/16/uncovered-active.bin"
	writeObj(t, root, rel)
	old := time.Now().Add(-48 * time.Hour)
	b := seedBlob(t, g, rel, model.BlobTrashed, &old)
	// active 行引用该路径、**无账本 ref**（模拟回填失败的残余）
	fc := &model.FileCode{Code: "UNCACT", FilePath: rel, Text: "x"}
	require.NoError(t, g.Create(fc).Error)

	n, err := j.ReclaimBlobs(ctx, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "未覆盖 active 行必须挡住回收（安全网②）")
	assert.FileExists(t, filepath.Join(root, filepath.FromSlash(rel)))
	var got model.Blob
	require.NoError(t, g.First(&got, b.ID).Error)
	assert.Equal(t, model.BlobTrashed, got.State, "状态不得推进")
}

// 收紧生效：active 行**有**账本 ref（released）⇒ 路径守卫不再重复扫描，
// 交守卫①（released 不计存活 ⇒ 0）——旧口径此场景被路径守卫挡住不回收。
//
// 前提（写路径不变量）：active 行 ⇒ active ref（启动回填+双写保证）；
// 「active 行 + released ref」属契约外状态，正常数据不存在，本用例直接构造
// 以钉住两个口径的行为差异；该形态的监控由对账扫描（#215）与写路径事务
// （行删与释放同事务，#207）承担。
func TestReclaimBlobs_CoveredActiveRowNotBlockedByPathGuard(t *testing.T) {
	root, j, g := newBlobGCTestEnv(t)
	ctx := context.Background()
	const rel = "uploads/2026/11/16/covered-active.bin"
	writeObj(t, root, rel)
	old := time.Now().Add(-48 * time.Hour)
	b := seedBlob(t, g, rel, model.BlobTrashed, &old)
	// active 行 + released ref（覆盖成立：行有账本 ref，只是非存活）
	fc := &model.FileCode{Code: "COVACT", FilePath: rel, Text: "x"}
	require.NoError(t, g.Create(fc).Error)
	require.NoError(t, g.Create(&model.FileReference{
		BlobID: b.ID, FileCodeID: fc.ID, State: model.RefReleased,
	}).Error)

	n, err := j.ReclaimBlobs(ctx, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "收紧生效：已覆盖 active 行不再被路径守卫重复挡住（守卫①=0 即可回收）")
	assert.NoFileExists(t, filepath.Join(root, filepath.FromSlash(rel)))
	var got model.Blob
	require.NoError(t, g.First(&got, b.ID).Error)
	assert.Equal(t, model.BlobPurged, got.State)
}
