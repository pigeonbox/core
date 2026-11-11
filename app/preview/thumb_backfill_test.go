// core#139 判据：存量空 Thumbnail 行必须补图。
//
// 修复前：get-or-create 以「file_previews 行存在」为缓存命中 ⇒ 修 core#131
// 「缩略图从未落盘」缺陷之前生成的 image 行（thumbnail 恒空）永远不会重新
// 生成，前端图片缩略图对存量分享永远缺失（core#131 疑虑 1）。
//
// 判据（断言结构事实）：
//   - image 行 thumbnail 空 ⇒ 视为未缓存，重新生成并**原地更新**既有行
//     （行 ID 不变、行数不增，与 core#117 janitor 行级映射对账兼容）；
//   - 补图幂等：再取一次走缓存命中，行不裂、值不漂；
//   - text 行空 Thumbnail 是设计态 ⇒ 不触发重生成（存量文本摘要不被覆盖）。
//
// 负对照（实做）：把 previewThumbMissing 回退为恒 false（旧行为）⇒ 第一条
// 判据转红（Thumbnail 仍空）。
package preview

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	previewService "github.com/pigeonbox/core/preview"
	"github.com/pigeonbox/core/repo/db/model"
)

// newBackfillEnv 真实预览引擎 + 内存库 + stubStorage（复用 thumbnail_persist_test
// 的 stubStorage）。
func newBackfillEnv(t *testing.T) (*Service, *gorm.DB, string) {
	t.Helper()
	cacheDir := t.TempDir()
	prev := previewService.GetService()
	t.Cleanup(func() { previewService.SetService(prev) })
	require.NoError(t, previewService.InitService(&previewService.Config{
		EnablePreview:    true,
		ThumbnailWidth:   120,
		ThumbnailHeight:  80,
		MaxFileSize:      50 * 1024 * 1024,
		PreviewCachePath: cacheDir,
		FFmpegPath:       "ffmpeg",
	}))

	gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, gormDB.AutoMigrate(&model.FileCode{}, &model.FilePreview{}))
	sqlDB, err := gormDB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return NewService(gormDB, &stubStorage{data: pngFixture(t)}), gormDB, cacheDir
}

func pngFixture(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 40, 20))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

func TestGetOrCreate_BackfillsLegacyEmptyThumbImageRow(t *testing.T) {
	svc, gormDB, cacheDir := newBackfillEnv(t)
	ctx := context.Background()
	fc := &model.FileCode{
		Code: "BACKFIL1", Text: "photo.png",
		FilePath: "uploads/2026/11/11/photo.png", UUIDFileName: "photo.png",
		Size: 40 * 20 * 4, ExpiredCount: 3,
	}
	require.NoError(t, gormDB.Create(fc).Error)
	// 存量坏行：修 core#131 之前生成的 image 行，thumbnail 恒空
	legacy := &model.FilePreview{FileCodeID: fc.ID, PreviewType: "image", Thumbnail: ""}
	require.NoError(t, gormDB.Create(legacy).Error)
	require.Empty(t, legacy.Thumbnail)

	got, err := svc.GetOrCreate(ctx, GetReq{Code: "BACKFIL1", ClientIP: "1.2.3.4"})
	require.NoError(t, err)

	// ① 补图：响应给可取 URL，DB 行（同一行）指向真实落盘缩略图
	assert.Equal(t, "/preview/BACKFIL1/thumb", got.Thumbnail, "响应面必须是可取 URL")
	var row model.FilePreview
	require.NoError(t, gormDB.First(&row, legacy.ID).Error, "必须原地更新既有行（行 ID 不变）")
	require.NotEmpty(t, row.Thumbnail, "存量空 Thumbnail 行必须补图（缺陷本体）")
	require.Contains(t, filepath.Base(row.Thumbnail), "_thumb", "janitor 对账只认 _thumb 标记")
	fi, err := os.Stat(filepath.Join(cacheDir, filepath.Base(row.Thumbnail)))
	require.NoError(t, err, "缩略图必须落盘")
	require.False(t, fi.IsDir())
	assert.Equal(t, 40, row.Width, "重新生成须带回真实解码尺寸")

	var count int64
	require.NoError(t, gormDB.Model(&model.FilePreview{}).Count(&count).Error)
	assert.EqualValues(t, 1, count, "补图不得裂行（行级映射唯一）")

	// ② 幂等：再取一次走缓存命中，值不漂
	got2, err := svc.GetOrCreate(ctx, GetReq{Code: "BACKFIL1", ClientIP: "1.2.3.4"})
	require.NoError(t, err)
	assert.Equal(t, "/preview/BACKFIL1/thumb", got2.Thumbnail)
	var row2 model.FilePreview
	require.NoError(t, gormDB.First(&row2, legacy.ID).Error)
	assert.Equal(t, row.Thumbnail, row2.Thumbnail, "已补图后不得再触发重生成/改写")
	require.NoError(t, gormDB.Model(&model.FilePreview{}).Count(&count).Error)
	assert.EqualValues(t, 1, count)
}

// TestGetOrCreate_TextRowEmptyThumbIsDesignNotStale text/code 无缩略图是设计态：
// 空 Thumbnail 不得触发重生成（否则每次请求都重读文件、覆盖存量文本摘要）。
func TestGetOrCreate_TextRowEmptyThumbIsDesignNotStale(t *testing.T) {
	svc, gormDB, _ := newBackfillEnv(t)
	ctx := context.Background()
	require.NoError(t, gormDB.Create(&model.FileCode{
		Code: "TEXTROW1", Text: "note.txt",
		FilePath: "uploads/note.txt", UUIDFileName: "note.txt",
		Size: 10, ExpiredCount: 3,
	}).Error)
	require.NoError(t, gormDB.Create(&model.FilePreview{
		FileCodeID: 1, PreviewType: "text", Thumbnail: "",
		TextContent: "MARKER-LEGACY-TEXT",
	}).Error)

	got, err := svc.GetOrCreate(ctx, GetReq{Code: "TEXTROW1", ClientIP: "1.2.3.4"})
	require.NoError(t, err)
	assert.Empty(t, got.Thumbnail, "text 行本就无缩略图")
	var row model.FilePreview
	require.NoError(t, gormDB.First(&row).Error)
	assert.Equal(t, "MARKER-LEGACY-TEXT", row.TextContent,
		"空 Thumbnail 的 text 行必须仍走缓存命中（重生成会覆盖存量摘要）")
}
