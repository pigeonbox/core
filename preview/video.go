package preview

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/oekit/pkg/convert"
)

// probeTimeout 外部进程单次调用上限（2026-10-08 加固）：恶意/损坏媒体文件可让
// ffmpeg/ffprobe 解析挂死——此前 getVideoInfo 起子进程不带 ctx，两个
// worker 可被无限期占用。帧提取同理封顶。
const probeTimeout = 30 * time.Second

// VideoGenerator 视频预览生成器
type VideoGenerator struct {
	config *Config
}

// NewVideoGenerator 创建视频生成器
func NewVideoGenerator(cfg *Config) *VideoGenerator {
	return &VideoGenerator{config: cfg}
}

// Generate 生成视频预览
func (g *VideoGenerator) Generate(ctx context.Context, filePath string, ext string) (*PreviewData, error) {
	// 获取视频信息
	info, err := g.getVideoInfo(ctx, filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to get video info: %w", err)
	}

	// 生成缩略图
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
		Type:      PreviewTypeVideo,
		Thumbnail: thumbnailPath,
		Width:     info.Width,
		Height:    info.Height,
		Duration:  info.Duration,
		FileSize:  fileSize,
		MimeType:  "video/" + strings.TrimPrefix(ext, "."),
		Extension: ext,
	}, nil
}

// SupportedTypes 支持的文件类型
func (g *VideoGenerator) SupportedTypes() []string {
	return []string{".mp4", ".avi", ".mov", ".wmv", ".flv", ".mkv", ".webm"}
}

// GenerateThumbnail 生成视频缩略图
func (g *VideoGenerator) GenerateThumbnail(ctx context.Context, filePath string, targetWidth, targetHeight int) (string, error) {
	// ffmpeg 路径推导收口 probePaths（与 ffprobe 同一份逻辑，core#84）
	ffmpegPath, _ := probePaths(g.config)

	// 构建缩略图路径
	fileName := filepath.Base(filePath)
	baseName := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	thumbnailFileName := fmt.Sprintf("%s_thumb.jpg", baseName)
	thumbnailPath := filepath.Join(g.config.PreviewCachePath, thumbnailFileName)

	// 确保缓存目录存在
	if err := os.MkdirAll(g.config.PreviewCachePath, 0755); err != nil {
		return "", fmt.Errorf("failed to create cache directory: %w", err)
	}

	// 使用ffmpeg提取第1秒的帧作为缩略图。-ss 前置到 -i 之前走关键帧快进定位，
	// 否则 ffmpeg 从 0 帧开始解码丢弃（长视频纯烧 CPU）
	args := []string{
		"-ss", "00:00:01", // 第1秒
		"-i", filePath,
		"-vframes", "1",
		"-vf", fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease", targetWidth, targetHeight),
		"-y", // 覆盖已存在的文件
		thumbnailPath,
	}

	// 上限封顶：调用方 ctx 无 deadline 时补 30s（恶意/损坏文件可挂死解析）；
	// 环境 allowlist + 有界输出捕获统一走 runProbeOutput（core#84）：
	// ffmpeg 继承全量环境 = 凭据泄漏面，CombinedOutput 无上限 = stderr 刷屏吃满内存
	ctx, cancel := probeCtx(ctx)
	defer cancel()
	_, stderrTail, err := runProbeOutput(ctx, ffmpegPath, args...)
	if err != nil {
		return "", fmt.Errorf("ffmpeg failed: %w, output: %s", err, truncateErr(stderrTail))
	}

	return thumbnailPath, nil
}

// VideoInfo 视频信息
type VideoInfo struct {
	Width    int
	Height   int
	Duration int // 秒
}

// getVideoInfo 获取视频信息（ffprobe 调用全部带 30s 超时）
func (g *VideoGenerator) getVideoInfo(ctx context.Context, filePath string) (*VideoInfo, error) {
	_, ffprobePath := probePaths(g.config)

	ctx, cancel := probeCtx(ctx)
	defer cancel()

	// 获取视频时长（stdout 是数据要单独取；stderr 只进诊断）
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

	// 获取视频分辨率
	resolutionTail, _, err := runProbeOutput(ctx, ffprobePath,
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height",
		"-of", "csv=s=x:p=0",
		filePath,
	)
	if err != nil {
		return nil, err
	}

	parts := strings.Split(strings.TrimSpace(resolutionTail), "x")
	var width, height int
	if len(parts) == 2 {
		width, _ = strconv.Atoi(parts[0])
		height, _ = strconv.Atoi(parts[1])
	}

	return &VideoInfo{
		Width:    width,
		Height:   height,
		Duration: int(duration),
	}, nil
}
