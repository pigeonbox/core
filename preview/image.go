package preview

import (
	"context"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/disintegration/imaging"

	// bmp/webp 解码器（core#139）：SupportedTypes 声明支持 .bmp/.webp。webp 此前
	// 无任何解码器注册 ⇒ image.Decode 必失败——声明的能力是假的（core#131 疑虑 2）；
	// bmp 此前经 disintegration/imaging 的传递 import（golang.org/x/image/bmp）
	// 侥幸可用，此处显式化：能力声明不得依赖第三方库的传递注册。
	// x/image 的 bmp/webp 均注册 image.RegisterFormat，blank-import 即接入
	// Decode/DecodeConfig 全链路。
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
)

// 解压炸弹守卫（2026-10-08 加固）：恶意 PNG/JPEG 可用极小压缩体积声明超大
// 像素矩阵，image.Decode 按头声明分配 RGBA——50MB 压缩包可膨胀成数 GB 内存。
// image.DecodeConfig 只读文件头即得宽高，超限直接拒绝，不做全量解码。
const (
	// MaxImagePixels 总像素上限（30M 像素 ≈ 解码后 120MB RGBA 峰值，覆盖所有常规摄影分辨率）
	MaxImagePixels int64 = 30_000_000
	// MaxImageDimension 单边上限（全景图 sanity，防极端长宽触发下游缩放算法异常）
	MaxImageDimension = 20000
)

// decodeImageGuarded 守卫式解码：先 DecodeConfig 校验头声明尺寸，再全量解码
// （Seek 回头）。所有图片解码路径必须经此入口。
func decodeImageGuarded(f *os.File) (image.Image, string, error) {
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return nil, "", fmt.Errorf("failed to probe image header: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, "", fmt.Errorf("invalid image dimensions %dx%d", cfg.Width, cfg.Height)
	}
	if cfg.Width > MaxImageDimension || cfg.Height > MaxImageDimension {
		return nil, "", fmt.Errorf("image dimension %dx%d exceeds limit %d", cfg.Width, cfg.Height, MaxImageDimension)
	}
	if int64(cfg.Width)*int64(cfg.Height) > MaxImagePixels {
		return nil, "", fmt.Errorf("image %d pixels exceeds limit %d", int64(cfg.Width)*int64(cfg.Height), MaxImagePixels)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, "", fmt.Errorf("rewind image file: %w", err)
	}
	return image.Decode(f)
}

// ImageGenerator 图片预览生成器
type ImageGenerator struct {
	config *Config
}

// NewImageGenerator 创建图片生成器
func NewImageGenerator(cfg *Config) *ImageGenerator {
	return &ImageGenerator{config: cfg}
}

// Generate 生成图片预览
func (g *ImageGenerator) Generate(ctx context.Context, filePath string, ext string) (*PreviewData, error) {
	// 打开图片文件
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open image: %w", err)
	}
	defer func() { _ = file.Close() }()

	// 守卫式解码（头声明尺寸超限即拒，防解压炸弹）；解码结果直接复用给缩略图，
	// 不再二次全量解码（此前 imaging.Open 重复解码同一文件）
	img, format, err := decodeImageGuarded(file)
	if err != nil {
		return nil, err
	}

	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()

	// 获取文件信息
	fileInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to get file info: %w", err)
	}

	// 生成缩略图（复用已解码图像）
	thumbnailPath, err := g.thumbnailFromImage(img, format, filePath)
	if err != nil {
		// 缩略图生成失败不影响主流程
		thumbnailPath = ""
	}

	return &PreviewData{
		Type:      PreviewTypeImage,
		Thumbnail: thumbnailPath,
		Width:     width,
		Height:    height,
		FileSize:  fileInfo.Size(),
		MimeType:  fmt.Sprintf("image/%s", format),
		Extension: ext,
	}, nil
}

// SupportedTypes 支持的文件类型
func (g *ImageGenerator) SupportedTypes() []string {
	return []string{".jpg", ".jpeg", ".png", ".gif", ".bmp", ".webp"}
}

// GenerateThumbnail 生成缩略图（Generator 接口方法；独立调用路径同样走守卫解码）
func (g *ImageGenerator) GenerateThumbnail(ctx context.Context, filePath string, targetWidth, targetHeight int) (string, error) {
	// 打开原图（守卫式：头声明尺寸超限直接拒绝，不做全量解码）
	f, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open image: %w", err)
	}
	defer func() { _ = f.Close() }()

	img, format, err := decodeImageGuarded(f)
	if err != nil {
		return "", err
	}
	return g.thumbnailFromImage(img, format, filePath)
}

// thumbnailSaveFormat 确定缩略图的保存格式与扩展名：以**解码得到的内容格式**
// 为真相源（core#131）。
//
// 生成链路把源文件暂存成无扩展名的 pb-preview-* 临时名再生成缩略图，此前保存
// 格式按**输入文件名的扩展名**推断（imaging.Save ⇒ FormatFromFilename）⇒ 暂存名
// 无扩展名恒 ErrUnsupportedFormat，且 Generate 吞错回空 ⇒ 图片缩略图从未落盘。
// 内容格式是唯一不依赖文件名的真相源；未识别时回退文件扩展名，仍不识别则默认
// PNG（imaging 可编码 ⇒ 保底落盘，不再静默空手而归）。
func thumbnailSaveFormat(contentFormat, filePath string) (imaging.Format, string) {
	switch strings.ToLower(contentFormat) {
	case "jpeg", "jpg":
		return imaging.JPEG, ".jpg"
	case "png":
		return imaging.PNG, ".png"
	case "gif":
		return imaging.GIF, ".gif"
	case "tif", "tiff":
		return imaging.TIFF, ".tif"
	case "bmp":
		return imaging.BMP, ".bmp"
	}
	if f, err := imaging.FormatFromFilename(filePath); err == nil {
		switch f {
		case imaging.JPEG:
			return imaging.JPEG, ".jpg"
		case imaging.PNG:
			return imaging.PNG, ".png"
		case imaging.GIF:
			return imaging.GIF, ".gif"
		case imaging.TIFF:
			return imaging.TIFF, ".tif"
		case imaging.BMP:
			return imaging.BMP, ".bmp"
		}
	}
	return imaging.PNG, ".png"
}

// thumbnailFromImage 从已解码图像生成缩略图落盘（不再重新解码原图）。
// contentFormat 为 image.Decode 报告的内容格式，保存格式由它显式指定
// （见 thumbnailSaveFormat）；文件名保留 _thumb 标记（janitor 对账口径）。
func (g *ImageGenerator) thumbnailFromImage(img image.Image, contentFormat, filePath string) (string, error) {
	// 生成缩略图（保持宽高比）
	thumbnail := imaging.Resize(img, g.config.ThumbnailWidth, 0, imaging.Lanczos)

	// 构建缩略图路径
	format, ext := thumbnailSaveFormat(contentFormat, filePath)
	fileName := filepath.Base(filePath)
	baseName := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	thumbnailFileName := fmt.Sprintf("%s_thumb%s", baseName, ext)
	thumbnailPath := filepath.Join(g.config.PreviewCachePath, thumbnailFileName)

	// 确保缓存目录存在
	if err := os.MkdirAll(g.config.PreviewCachePath, 0755); err != nil {
		return "", fmt.Errorf("failed to create cache directory: %w", err)
	}

	// 保存缩略图：显式按格式编码（不再走 imaging.Save 的「按文件名猜格式」——
	// 暂存输入无扩展名时那条路恒 ErrUnsupportedFormat）
	f, err := os.Create(thumbnailPath)
	if err != nil {
		return "", fmt.Errorf("failed to create thumbnail: %w", err)
	}
	if err := imaging.Encode(f, thumbnail, format); err != nil {
		_ = f.Close()
		_ = os.Remove(thumbnailPath)
		return "", fmt.Errorf("failed to save thumbnail: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("failed to save thumbnail: %w", err)
	}

	return thumbnailPath, nil
}
