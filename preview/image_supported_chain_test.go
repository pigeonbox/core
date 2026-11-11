// core#139 判据：「能力不得撒谎」——ImageGenerator.SupportedTypes 声明支持的
// 每种扩展名，都必须真能走通预览链路（解码 + 缩略图落盘）。
//
// 修复前：SupportedTypes 含 .bmp/.webp，但解码器未注册（只 blank-import 了
// 标准库 gif/jpeg/png）⇒ image.Decode 必失败 ⇒ 这两类扩展名实际不可预览
// （core#131 疑虑 2）。
//
// 覆盖闭合：SupportedTypes 的每个条目都必须有 fixture（新增声明类型漏素材
// 即红）；fixture 的扩展名也必须都在 SupportedTypes 里（素材与声明不脱节）。
//
// webp 无编码器（x/image/webp 只有解码）⇒ fixture 为 cwebp 产出的
// testdata 真实码流（VP8L 无损 + VP8 有损各一，钉住两条解码路径）。
//
// ⚠️ 判定修正（核对依赖图后）：.bmp 修前其实**可解码**——disintegration/imaging
// 的 io.go 传递 import 了 golang.org/x/image/bmp（连带 tiff），image.RegisterFormat
// 随包注册 ⇒ 「解不了码」的只有 .webp。本批仍把 x/image/bmp 的注册显式化
// （preview/image.go 直接 blank-import）：能力声明不应依赖第三方库的传递
// import（imaging 哪天去掉 bmp，SupportedTypes 就悄悄变回撒谎）。bmp fixture
// 用 imaging.Encode 构造、不直接 import x/image/bmp，避免测试自身注册解码器
// 掩盖判定来源。
//
// 负对照（实做）：撤掉 preview/image.go 的 x/image bmp/webp blank-import ⇒
// .webp 两个子用例转红（image: unknown format）；.bmp 因 imaging 传递注册仍绿
// （依赖图事实，非判据缺陷）。
package preview

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/disintegration/imaging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildImageFixture 生成 16x8 测试图内容（格式与扩展名一致）。
func buildImageFixture(t *testing.T, ext string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 8))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	switch ext {
	case ".jpg", ".jpeg":
		require.NoError(t, jpeg.Encode(&buf, img, nil))
	case ".png":
		require.NoError(t, png.Encode(&buf, img))
	case ".gif":
		require.NoError(t, gif.Encode(&buf, img, nil))
	case ".bmp":
		// imaging 的 BMP 编码（勿改用 x/image/bmp.Encode：测试 import 它会
		// 把 bmp 解码器注册进测试二进制，掩盖「注册来源」判定）
		require.NoError(t, imaging.Encode(&buf, img, imaging.BMP))
	default:
		t.Fatalf("fixture 构造器缺 %s 的实现（SupportedTypes 新增类型必须补素材）", ext)
	}
	return buf.Bytes()
}

func TestImageGenerator_SupportedTypesAllDecodable(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		EnablePreview:    true,
		ThumbnailWidth:   120,
		ThumbnailHeight:  80,
		MaxFileSize:      50 * 1024 * 1024,
		PreviewCachePath: dir,
		FFmpegPath:       "ffmpeg",
	}
	engine := NewService(cfg)
	engine.RegisterGenerator(PreviewTypeImage, NewImageGenerator(cfg))
	ctx := context.Background()

	type spec struct {
		ext  string
		file string // fixture 文件名（写入临时目录）
		data []byte
	}
	var specs []spec
	for _, ext := range NewImageGenerator(cfg).SupportedTypes() {
		switch ext {
		case ".webp":
			for _, name := range []string{"sample-lossless.webp", "sample-lossy.webp"} {
				data, err := os.ReadFile(filepath.Join("testdata", name))
				require.NoError(t, err, "webp fixture 缺失：%s", name)
				specs = append(specs, spec{ext: ext, file: name, data: data})
			}
		default:
			specs = append(specs, spec{ext: ext, file: "sample" + ext, data: buildImageFixture(t, ext)})
		}
	}
	// 覆盖闭合：SupportedTypes 每个声明类型至少一个 fixture（上面 default 分支
	// 遇未知扩展名即 Fatal，此处再断言数量下界防漏）
	require.GreaterOrEqual(t, len(specs), len(NewImageGenerator(cfg).SupportedTypes()),
		"SupportedTypes 的每个声明类型都必须有可解码素材")

	for _, s := range specs {
		t.Run(s.file, func(t *testing.T) {
			path := filepath.Join(dir, s.file)
			require.NoError(t, os.WriteFile(path, s.data, 0644))

			pd, err := engine.GeneratePreview(ctx, path, s.ext)
			require.NoError(t, err, "SupportedTypes 声明 %s 可预览，就必须真能走通", s.ext)
			require.Equal(t, PreviewTypeImage, pd.Type)
			assert.Equal(t, 16, pd.Width, "源图尺寸必须被真实解码")
			assert.Equal(t, 8, pd.Height)

			require.NotEmpty(t, pd.Thumbnail, "%s 的缩略图必须落盘（声明能预览却无产物=能力撒谎）", s.ext)
			f, err := os.Open(pd.Thumbnail)
			require.NoError(t, err)
			defer func() { _ = f.Close() }()
			cfgImg, _, err := image.DecodeConfig(f)
			require.NoError(t, err, "缩略图必须是可解码的图像字节")
			assert.Equal(t, 120, cfgImg.Width, "缩略图按 ThumbnailWidth 缩放")
		})
	}
}
