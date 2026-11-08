package preview

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/oekit/pkg/convert"
)

// AudioGenerator 音频预览生成器
type AudioGenerator struct {
	config *Config
}

// NewAudioGenerator 创建音频生成器
func NewAudioGenerator(cfg *Config) *AudioGenerator {
	return &AudioGenerator{config: cfg}
}

// Generate 生成音频预览
func (g *AudioGenerator) Generate(ctx context.Context, filePath string, ext string) (*PreviewData, error) {
	// 获取音频信息
	info, err := g.getAudioInfo(ctx, filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to get audio info: %w", err)
	}

	// 生成波形缩略图（失败容忍回空，前端以默认音频图标兜底——与 video 截帧同款）
	thumbnailPath, err := g.GenerateThumbnail(ctx, filePath, g.config.ThumbnailWidth, g.config.ThumbnailHeight)
	if err != nil {
		thumbnailPath = ""
	}

	// 获取文件大小
	fileInfo, _ := os.Stat(filePath)
	var fileSize int64
	if fileInfo != nil {
		fileSize = fileInfo.Size()
	}

	return &PreviewData{
		Type:      PreviewTypeAudio,
		Thumbnail: thumbnailPath,
		Duration:  info.Duration,
		FileSize:  fileSize,
		MimeType:  "audio/" + strings.TrimPrefix(ext, "."),
		Extension: ext,
	}, nil
}

// SupportedTypes 支持的文件类型
func (g *AudioGenerator) SupportedTypes() []string {
	return []string{".mp3", ".wav", ".flac", ".aac", ".ogg", ".m4a"}
}

// GenerateThumbnail 生成音频波形图（ffmpeg showwavespic，宽高按生成器配置
// 的缩略图尺寸）。ffmpeg 不可用或生成失败时回空——波形是可选增强，前端
// 以默认音频图标兜底，不因波形失败判整个预览不可用。
func (g *AudioGenerator) GenerateThumbnail(ctx context.Context, filePath string, width, height int) (string, error) {
	// ffmpeg 路径收口 probePaths（core#84）：此前硬编码按名找 PATH 不走
	// config.FFmpegPath ⇒ 配了绝对路径的部署波形仍找 PATH 里的二进制，
	// 与 video 口径不一致
	ffmpegPath, _ := probePaths(g.config)

	ctx, cancel := probeCtx(ctx)
	defer cancel()

	w := g.config.ThumbnailWidth
	h := g.config.ThumbnailHeight
	if width > 0 {
		w = width
	}
	if height > 0 {
		h = height
	}

	fileName := filepath.Base(filePath)
	baseName := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	// 波形图固定 png（无透明需求、色彩够用）；命名与 image 生成器同约定
	outPath := filepath.Join(g.config.PreviewCachePath, fmt.Sprintf("%s_thumb.png", baseName))
	if err := os.MkdirAll(g.config.PreviewCachePath, 0755); err != nil {
		return "", fmt.Errorf("failed to create cache directory: %w", err)
	}

	// showwavespic 出单帧波形图；colors 用品牌科技蓝色做前景（波形是 UI 素材）。
	// 环境 allowlist + 有界输出捕获统一走 runProbeOutput（core#84），与 video 同款
	_, stderrTail, err := runProbeOutput(ctx, ffmpegPath,
		"-y", "-i", filePath,
		"-filter_complex", fmt.Sprintf("showwavespic=s=%dx%d:colors=0x1677ff", w, h),
		"-frames:v", "1", outPath,
	)
	if err != nil {
		return "", fmt.Errorf("waveform generation failed: %w (%s)", err, truncateErr(stderrTail))
	}
	return outPath, nil
}

// truncateErr 截断 ffmpeg 输出尾部（失败诊断用，防超长 stderr 灌日志）。
// 入参已是 runProbeOutput 的有界尾部（≤probeOutputCap），再截 200 字符进日志。
func truncateErr(s string) string {
	const max = 200
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[len(s)-max:]
	}
	return s
}

// AudioInfo 音频信息
type AudioInfo struct {
	Duration   int // 秒
	BitRate    int
	SampleRate int
}

// getAudioInfo 获取音频信息（ffprobe 30s 超时，防损坏文件挂死 worker）
func (g *AudioGenerator) getAudioInfo(ctx context.Context, filePath string) (*AudioInfo, error) {
	// ffprobe 路径同口径收口 probePaths（此前硬编码，core#84）
	_, ffprobePath := probePaths(g.config)

	ctx, cancel := probeCtx(ctx)
	defer cancel()

	// 获取音频时长（stdout 是数据要单独取；stderr 只进诊断）
	durationTail, _, err := runProbeOutput(ctx, ffprobePath,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		filePath,
	)
	if err != nil {
		return nil, err
	}
	duration := convert.Str2Float64(strings.TrimSpace(durationTail), 0)

	return &AudioInfo{
		Duration: int(duration),
	}, nil
}
