package preview

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeFakePNG 构造只有 IHDR 头的 PNG（不写像素数据）：DecodeConfig 只读头即可
// 拿到声明尺寸，用于验证解压炸弹守卫在Allocation 发生前拒绝。
func makeFakePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write([]byte("\x89PNG\r\n\x1a\n"))
	chunk := func(typ string, data []byte) {
		_ = binary.Write(&buf, binary.BigEndian, uint32(len(data)))
		buf.WriteString(typ)
		buf.Write(data)
		_ = binary.Write(&buf, binary.BigEndian, crc32.ChecksumIEEE(append([]byte(typ), data...)))
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], uint32(w))
	binary.BigEndian.PutUint32(ihdr[4:8], uint32(h))
	ihdr[8] = 8 // bit depth
	ihdr[9] = 6 // color type RGBA
	chunk("IHDR", ihdr)
	chunk("IEND", nil)
	return buf.Bytes()
}

// TestDecodeImageGuarded 解压炸弹守卫（2026-10-08 加固）：头声明尺寸超限必须在
// 全量解码（内存分配）前被拒绝。
func TestDecodeImageGuarded(t *testing.T) {
	dir := t.TempDir()

	// 超总像素（40000×40000 = 16 亿像素）
	bomb := filepath.Join(dir, "bomb.png")
	if err := os.WriteFile(bomb, makeFakePNG(t, 40000, 40000), 0644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(bomb)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, _, err := decodeImageGuarded(f); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("超大声明头应被拒绝，got err=%v", err)
	}

	// 正常小图必须放行
	small := filepath.Join(dir, "small.png")
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(small, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	sf, err := os.Open(small)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sf.Close() }()
	img, format, err := decodeImageGuarded(sf)
	if err != nil {
		t.Fatalf("正常小图不应被拒绝: %v", err)
	}
	if format != "png" || img.Bounds().Dx() != 4 {
		t.Fatalf("解码结果不符: format=%s bounds=%v", format, img.Bounds())
	}
}

// TestImageGenerator_Generate 生成链路：小图出预览数据+缩略图落盘；炸弹图拒绝。
func TestImageGenerator_Generate(t *testing.T) {
	dir := t.TempDir()
	g := NewImageGenerator(&Config{
		EnablePreview:    true,
		ThumbnailWidth:   100,
		ThumbnailHeight:  100,
		PreviewCachePath: dir,
	})

	// 正常图：生成成功且缩略图落盘
	src := filepath.Join(dir, "cat.png")
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	data, err := g.Generate(t.Context(), src, ".png")
	if err != nil {
		t.Fatalf("正常图生成失败: %v", err)
	}
	if data.Width != 8 || data.Thumbnail == "" {
		t.Fatalf("预览数据不符: %+v", data)
	}
	if _, err := os.Stat(data.Thumbnail); err != nil {
		t.Fatalf("缩略图未落盘: %v", err)
	}

	// 炸弹图：拒绝且不落缩略图
	bomb := filepath.Join(dir, "bomb.png")
	if err := os.WriteFile(bomb, makeFakePNG(t, 30000, 30000), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Generate(t.Context(), bomb, ".png"); err == nil {
		t.Fatal("炸弹图应被拒绝")
	}
}

// TestImageGenerator_Generate_StagedNoExtThumbPersists 缩略图落盘不得依赖
// 输入文件名的扩展名（回归锁，缺陷：图片缩略图从未落盘）。
//
// 生产链路（app/preview/service.go generatePreview）把源文件暂存成**无扩展名**
// 的 pb-preview-* 临时名再生成缩略图 ⇒ 缩略图保存格式若按文件名扩展名猜
// （imaging.Save ⇒ FormatFromFilename）⇒ 恒 ErrUnsupportedFormat，且 Generate
// 吞错回空 ⇒ Thumbnail 恒空、图片缩略图从未落盘（video/audio 固定后缀不受影响）。
//
// 本用例以生产同构的暂存名复现并锁修：缩略图必须真实落盘（可解码图像字节）
// 且文件名保留 _thumb 标记（janitor 对账口径）。
// 负对照（实做）：把保存格式改回「按文件名猜」⇒ data.Thumbnail 恒空 ⇒ 本用例红。
func TestImageGenerator_Generate_StagedNoExtThumbPersists(t *testing.T) {
	dir := t.TempDir()
	g := NewImageGenerator(&Config{
		EnablePreview:    true,
		ThumbnailWidth:   100,
		ThumbnailHeight:  100,
		PreviewCachePath: dir,
	})

	// 生产同构暂存：无扩展名的 pb-preview-* 临时名（app/preview 的 os.CreateTemp）
	staged := filepath.Join(dir, "pb-preview-1234567890")
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 16, 8))
	img.Set(0, 0, color.RGBA{G: 255, A: 255})
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}

	data, err := g.Generate(t.Context(), staged, ".png")
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if data.Thumbnail == "" {
		t.Fatal("缩略图为空：无扩展名暂存名 ⇒ 保存格式无法从文件名推断 ⇒ 缩略图从未落盘（缺陷本体）")
	}
	fi, err := os.Stat(data.Thumbnail)
	if err != nil || fi.IsDir() {
		t.Fatalf("缩略图未落盘: %v", err)
	}
	if !strings.Contains(filepath.Base(data.Thumbnail), "_thumb") {
		t.Fatalf("缩略图名必须带 _thumb 标记（janitor 对账只动带标记的文件）: %s", data.Thumbnail)
	}
	// 真实图像字节（不是空文件/错格式占位）
	f, err := os.Open(data.Thumbnail)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, _, err := image.DecodeConfig(f); err != nil {
		t.Fatalf("缩略图不是可解码图像: %v", err)
	}
}
