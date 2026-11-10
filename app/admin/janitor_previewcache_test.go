package admin

// core#117 预览缩略图缓存回收守卫（janitor.CleanupPreviewCache）。
//
// 判据（对账式）：缓存文件被 file_previews 行引用且其分享仍在（含回收站）⇒ 保留；
// 分享已 purge / 无主残留 ⇒ 超宽限期回收；行侧同轮清孤儿。
//
// 负对照（回退任一半 ⇒ 红）：
//   - 去掉「行引用」判定 ⇒ 存活分享的缓存被误删；
//   - 去掉「file_code_id 含软删存在」判定 ⇒ 回收站分享的缓存被误删（恢复后 404）。

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCleanupPreviewCache(t *testing.T) {
	_, svc, g := newJanitorTestEnv(t)
	require.NoError(t, g.AutoMigrate(&model.FilePreview{}))
	ctx := context.Background()
	cacheDir := t.TempDir()

	mkCache := func(name string, age time.Duration) string {
		t.Helper()
		p := filepath.Join(cacheDir, name)
		require.NoError(t, os.WriteFile(p, []byte("thumb"), 0o644))
		if age > 0 {
			old := time.Now().Add(-age)
			require.NoError(t, os.Chtimes(p, old, old))
		}
		return p
	}
	mkShare := func(code string) *model.FileCode {
		t.Helper()
		fc := &model.FileCode{Code: code}
		require.NoError(t, g.Create(fc).Error)
		return fc
	}

	// ① 存活分享 + 有行引用 ⇒ 保留
	alive := mkShare("PCALIVE1")
	require.NoError(t, g.Create(&model.FilePreview{
		FileCodeID: alive.ID, PreviewType: "video", Thumbnail: "alive_thumb.jpg",
	}).Error)
	aliveThumb := mkCache("alive_thumb.jpg", 48*time.Hour)

	// ② 回收站（软删）分享 + 有行引用 ⇒ 保留（恢复后还要出图）
	soft := mkShare("PCASOFT1")
	require.NoError(t, g.Create(&model.FilePreview{
		FileCodeID: soft.ID, PreviewType: "image", Thumbnail: "soft_thumb.png",
	}).Error)
	softThumb := mkCache("soft_thumb.png", 48*time.Hour)
	require.NoError(t, g.Delete(&model.FileCode{}, soft.ID).Error)

	// ③ 分享已 purge ⇒ 缓存删 + 行删（只写不删的存量泄漏正是这条）
	purged := mkShare("PCAPURG1")
	require.NoError(t, g.Create(&model.FilePreview{
		FileCodeID: purged.ID, PreviewType: "audio", Thumbnail: "purged_thumb.png",
	}).Error)
	purgedThumb := mkCache("purged_thumb.png", 48*time.Hour)
	require.NoError(t, g.Unscoped().Delete(&model.FileCode{}, purged.ID).Error)

	// ④ 无行引用的落盘残留（生成后行写失败）⇒ 删
	orphanThumb := mkCache("orphan_thumb.png", 48*time.Hour)

	// ⑤ 历史行存全路径 ⇒ Base 归一后同样保留
	fullPath := mkShare("PCAFULL1")
	require.NoError(t, g.Create(&model.FilePreview{
		FileCodeID: fullPath.ID, PreviewType: "video",
		Thumbnail: "/var/data/previews/full_thumb.jpg",
	}).Error)
	fullThumb := mkCache("full_thumb.jpg", 48*time.Hour)

	// ⑥ 宽限期内的孤儿（在途预览：先落盘后落行）⇒ 过
	freshThumb := mkCache("fresh_thumb.png", 0)

	// ⑦ 非生成器产物（无 _thumb 标记）⇒ 永远不动
	stray := mkCache("stray-file.bin", 48*time.Hour)

	j := NewJanitor(g, svc, 90)
	files, rows, err := j.CleanupPreviewCache(ctx, cacheDir, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 2, files, "只清 purge 后的缓存 + 无主残留")
	assert.EqualValues(t, 1, rows, "只清主表已 purge 的预览行")

	assert.FileExists(t, aliveThumb, "存活分享的缓存不得误删")
	assert.FileExists(t, softThumb, "回收站分享的缓存不得误删（恢复语义）")
	assert.NoFileExists(t, purgedThumb, "purge 后的缩略图必须回收")
	assert.NoFileExists(t, orphanThumb, "无主残留必须回收")
	assert.FileExists(t, fullThumb, "历史全路径行按 Base 归一保留")
	assert.FileExists(t, freshThumb, "宽限期内（在途预览）不得删")
	assert.FileExists(t, stray, "非 _thumb 文件不动（宁可漏删不误删）")

	// 行侧：purge 分享的行删掉，软删/存活分享的行保留
	var cnt int64
	require.NoError(t, g.Unscoped().Model(&model.FilePreview{}).Count(&cnt).Error)
	assert.EqualValues(t, 3, cnt, "alive/soft/full 三行保留，purged 行已删")

	// 幂等：再跑一轮零清理
	files, rows, err = j.CleanupPreviewCache(ctx, cacheDir, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 0, files)
	assert.EqualValues(t, 0, rows)
}
