// NAS 本地文件免上传导入（P3）：把服务器本地白名单目录内的文件登记为分享。
// 服务端从本地路径拷贝入统一存储（不经过浏览器上传通道），配额/审核/过期
// 与普通上传同链路。典型场景：fnOS/NAS 上已有的大文件直接生成提取码。
package share

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/logger"
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
}

// ImportLocalFile 把本地文件导入为分享。
// 校验链：功能开关 → 路径落在白名单目录内（EvalSymlinks 防穿越）→ 普通文件 →
// 大小上限 → 服务端拷贝入存储 → 复用 CreateShare（配额/审核/自定义码）。
func (s *Service) ImportLocalFile(ctx context.Context, opts ImportLocalOpts) (*ShareResp, error) {
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
	info, err := os.Stat(real)
	if err != nil {
		return nil, fmt.Errorf("文件不可访问: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("仅支持导入普通文件")
	}
	if err := utils.CheckUploadSize(info.Size(), utils.GetMaxFileSize()); err != nil {
		return nil, utils.NewFileTooLargeError(utils.GetMaxFileSize())
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
	channel := "local_import"
	share, serr := s.CreateShare(ctx, &ShareFileReq{
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
	if serr != nil {
		// ⚠️ 2026-10-26 审计修复：建分享失败（配额满/审核 reject/自定义码冲突）时
		// **已落盘的副本没有 DB 引用** ⇒ 本地后端 24h 后被对账清掉，但**远端后端
		// 永不被回收**（ReconcileOrphans 跳过远端，presign 孤儿扫描只认 up_ 前缀
		// ⇒ 本地导入用的是 UUID 名）⇒ 永久占空间。⇒ 失败即删副本。
		if derr := s.storage.DeleteFile(ctx, rel); derr != nil {
			logger.CtxWarn(ctx, "delete orphaned local-import copy failed",
				"rel", rel, "err", derr)
		}
		return nil, serr
	}
	return share, nil
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
