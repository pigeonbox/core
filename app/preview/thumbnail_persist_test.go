package preview

// 缩略图生产链路服务级回归锁（缺陷：图片缩略图从未落盘）+ 与 core#117 janitor
// 对账的往返核对。
//
// 生产链路把源文件暂存成无扩展名的 pb-preview-* 临时名再生成缩略图，保存格式
// 曾按文件名扩展名猜（imaging.Save）⇒ 恒 ErrUnsupportedFormat 被吞 ⇒ 图片
// 缩略图从未落盘。本用例走与生产一字不差的路径（generatePreview 内部即
// os.CreateTemp("pb-preview-*")）证明：
//   - 修复后缩略图真实落盘且 file_previews.thumbnail 行级映射指向它（core#117
//     janitor 对账的唯一真相源）；
//   - 该产物能被 admin.CleanupPreviewCache 正确「保留↔回收」：行引用 + 分享存活
//     ⇒ 保留（不与行级映射冲突）；分享 purge ⇒ 缓存与行同轮回收。
//
// 负对照（实做）：把 preview/image.go 的保存格式改回「按文件名猜」⇒ Thumbnail
// 恒空 ⇒ 第一条断言红。
import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/app/admin"
	previewService "github.com/pigeonbox/core/preview"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/pigeonbox/core/storage"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// stubStorage 只实现 generatePreview 触达的 GetFileReader（其余方法本用例不走；
// 内嵌 nil 接口占位，误触达即 panic 而不是静默假绿）。
type stubStorage struct {
	storage.StorageInterface
	data []byte
}

func (s *stubStorage) GetFileReader(_ context.Context, _ string) (io.ReadCloser, int64, error) {
	return io.NopCloser(bytes.NewReader(s.data)), int64(len(s.data)), nil
}

func TestGeneratePreview_ImageThumbPersistsAndJanitorRoundTrip(t *testing.T) {
	// 真实预览引擎（注册 image 生成器）+ 独立缓存目录
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

	// 内存库（钉单连接）
	gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, gormDB.AutoMigrate(&model.FileCode{}, &model.FilePreview{}))
	sqlDB, err := gormDB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	// 真 PNG 内容
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 40, 20))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	require.NoError(t, png.Encode(&buf, img))
	pngBytes := buf.Bytes()

	ctx := context.Background()
	fileCode := &model.FileCode{
		Code: "THUMBP1", Text: "photo.png",
		FilePath: "uploads/2026/11/11/photo.png", UUIDFileName: "photo.png",
		Size: int64(len(pngBytes)), ExpiredCount: 3,
	}
	require.NoError(t, gormDB.Create(fileCode).Error)

	svc := NewService(gormDB, &stubStorage{data: pngBytes})
	preview, err := svc.generatePreview(ctx, fileCode) // 内部暂存成 pb-preview-*（无扩展名）
	require.NoError(t, err)

	// ① 缩略图真实落盘
	require.NotEmpty(t, preview.Thumbnail, "Thumbnail 恒空 = 缩略图从未落盘（缺陷本体）")
	fi, err := os.Stat(preview.Thumbnail)
	require.NoError(t, err, "缩略图必须落盘")
	require.False(t, fi.IsDir())
	require.Contains(t, filepath.Base(preview.Thumbnail), "_thumb", "janitor 对账只认 _thumb 标记")

	// ② file_previews 行级映射指向落盘位置（core#117 janitor 的唯一真相源）
	row, err := svc.previewRepo.GetByFileCodeID(ctx, fileCode.ID)
	require.NoError(t, err)
	require.Equal(t, filepath.Base(preview.Thumbnail), filepath.Base(row.Thumbnail),
		"行级映射必须指向刚落盘的缩略图（Base 归一，与 janitor/GetThumb 同口径）")

	// ③ janitor 对账往返：行引用 + 分享存活 ⇒ 保留；purge ⇒ 缓存与行同轮回收
	old := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(preview.Thumbnail, old, old))
	j := admin.NewJanitor(gormDB, nil, 90)
	files, _, err := j.CleanupPreviewCache(ctx, cacheDir, 24*time.Hour)
	require.NoError(t, err)
	require.Equal(t, 0, files, "被行引用且分享存活的缩略图不得误删")
	require.FileExists(t, preview.Thumbnail)

	require.NoError(t, gormDB.Unscoped().Delete(&model.FileCode{}, fileCode.ID).Error)
	files, rows, err := j.CleanupPreviewCache(ctx, cacheDir, 24*time.Hour)
	require.NoError(t, err)
	require.Equal(t, 1, files, "分享 purge 后缩略图必须被对账回收")
	require.EqualValues(t, 1, rows, "孤儿预览行同轮清掉")
	require.NoFileExists(t, preview.Thumbnail)
}
