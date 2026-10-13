// NAS 本地文件免上传导入（P3）：把服务器本地白名单目录内的文件登记为分享。
// 服务端从本地路径拷贝入统一存储（不经过浏览器上传通道），配额/审核/过期
// 与普通上传同链路。典型场景：fnOS/NAS 上已有的大文件直接生成提取码。
package share

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/utils"
)

// ImportLocalOpts 本地导入参数
type ImportLocalOpts struct {
	AbsPath      string
	ExpireValue  int
	ExpireStyle  string
	RequireAuth  bool
	PasswordHash string
	CustomCode   string
	UserID       *uint
	OwnerIP      string
	// Internal 内部调用（P2a 设备直传发布为分享）：跳过 local_import 功能开关
	// 与其 roots 校验——调用方（p2px.PublishPaths）已按收件区做三层防穿越，
	// 且直传有独立总开关（p2p.direct.enabled）；仅 admin 链路可达。
	Internal bool
}

// ImportLocalFile 把本地文件导入为分享。
// 校验链：功能开关 → 路径落在白名单目录内（EvalSymlinks 防穿越）→ 普通文件 →
// 大小上限 → 服务端拷贝入存储 → 复用 CreateShare（配额/审核/自定义码）。
// Internal=true 时功能开关与 local_import roots 校验由调用方担保（见上注释）。
func (s *Service) ImportLocalFile(ctx context.Context, opts ImportLocalOpts) (*ShareResp, error) {
	if !opts.Internal {
		cfg := conf.GetGlobalConfig()
		if cfg == nil || !cfg.Upload.LocalImport.Enabled {
			return nil, errors.New("本地文件导入功能未启用")
		}
		// 路径规范化 + 符号链接解析（防 /allowed/root/../../etc/passwd 穿越）
		clean := filepath.Clean(opts.AbsPath)
		real, err := filepath.EvalSymlinks(clean)
		if err != nil {
			return nil, fmt.Errorf("文件不可访问: %w", err)
		}
		if !withinRoots(real, cfg.Upload.LocalImport.Roots) {
			return nil, errors.New("路径不在允许导入的目录内")
		}
		opts.AbsPath = real
	}
	// Internal 路径同样走 symlink 归一（调用方校验之外的纵深防御）
	clean := filepath.Clean(opts.AbsPath)
	real, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return nil, fmt.Errorf("文件不可访问: %w", err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return nil, fmt.Errorf("文件不可访问: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("仅支持导入普通文件")
	}
	if err := utils.CheckUploadSize(info.Size(), utils.GetMaxFileSize()); err != nil {
		return nil, fmt.Errorf("文件超过大小上限（%d 字节）", utils.GetMaxFileSize())
	}

	name := utils.SanitizeFileName(filepath.Base(real))
	if !utils.IsAllowedExtension(name) {
		return nil, errors.New("该文件类型禁止上传")
	}

	// 拷贝入统一存储（走 SaveStream，local/S3/webdav 通用）
	f, err := os.Open(real)
	if err != nil {
		return nil, fmt.Errorf("打开文件失败: %w", err)
	}
	defer func() { _ = f.Close() }()

	_, rel := utils.NewUploadRelPath(name)
	written, err := s.storage.SaveStream(ctx, rel, f, info.Size())
	if err != nil {
		return nil, fmt.Errorf("导入存储失败: %w", err)
	}

	// 整文件哈希（秒传；读已落盘副本）
	fileHash := ""
	if rc, _, rerr := s.storage.GetFileReader(ctx, rel); rerr == nil {
		fileHash, _ = utils.HashReader(rc)
		_ = rc.Close()
	}
	_ = io.Discard

	channel := "local_import"
	if opts.Internal {
		channel = "p2p_publish" // 直传发布：渠道归因区分（审计/统计可见）
	}
	return s.CreateShare(ctx, &ShareFileReq{
		Channel:      channel,
		FilePath:     rel,
		Size:         written,
		Text:         name,
		ExpiredAt:    utils.CalculateExpireTime(opts.ExpireValue, opts.ExpireStyle),
		ExpiredCount: utils.CalculateExpireCount(opts.ExpireStyle, opts.ExpireValue),
		RequireAuth:  opts.RequireAuth,
		PasswordHash: opts.PasswordHash,
		UserID:       opts.UserID,
		UploadType:   "authenticated",
		OwnerIP:      opts.OwnerIP,
		FileHash:     fileHash,
		CustomCode:   opts.CustomCode,
	})
}

// withinRoots 路径是否落在任一白名单目录内（含目录本身；均已 EvalSymlinks 化）
func withinRoots(realPath string, roots []string) bool {
	for _, root := range roots {
		rootReal, err := filepath.EvalSymlinks(filepath.Clean(root))
		if err != nil {
			continue
		}
		if realPath == rootReal || strings.HasPrefix(realPath, rootReal+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
