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
	"github.com/pigeonbox/core/pkg/gate"
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
// 校验链：功能开关 → 路径落在白名单目录内（EvalSymlinks 防穿越）→ 目录策略闸口
// （share_mode=deny 拒收；quota_bytes 超限亦拒收，两者都**在落盘前**、零副作用）
// → 普通文件 → 大小上限 → 服务端拷贝入存储 → 复用 CreateShare（站点/用户配额、
// 审核、自定义码）。
//
// 目录配额（core#159）：命中目录策略时，判定与在途预留发生在
// reserveDirectoryQuota 的临界区内（与站点/用户维度同一把锁 ⇒ 并发不超放），
// 预留持有到建行提交之后才释放。超限返回 *DirectoryQuotaExceededError
// （typed ⇒ errcode.CodeStorageQuota/30002 ⇒ HTTP 507）。
func (s *Service) ImportLocalFile(ctx context.Context, opts ImportLocalOpts) (*ShareResp, error) {
	// 次数型下界（core#220）：在读文件/拷贝入存储之前拒绝（零副作用）。
	if err := utils.CheckCountExpireValue(opts.ExpireStyle, opts.ExpireValue); err != nil {
		return nil, err
	}
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
	// 目录策略（core#159）：share_mode=deny 在此拒收（在落盘/拷贝之前，零副作用）。
	// 命中条目只解析一次：既用于 share_mode 闸口，也用于下面的目录配额归因
	// （命中条目的 Root 是**解析后**形态 ⇒ 归因 key 与文案里的目录一致）。
	matched := resolveDirectoryPolicyMatch(cfg.Share.DirectoryPolicies, real)
	if err := checkDirectoryPolicyMatch(matched); err != nil {
		return nil, err
	}
	opts.AbsPath = real
	info, err := os.Stat(real)
	if err != nil {
		return nil, fmt.Errorf("文件不可访问: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("仅支持导入普通文件")
	}
	if err := gate.CheckFileSize(info.Size()); err != nil {
		return nil, err
	}
	// 目录配额（core#159）：命中策略且 quota_bytes>0 ⇒ 在**落盘前**判定。
	// 超限 ⇒ 不落文件、不建行（typed error ⇒ errcode.CodeStorageQuota/30002）。
	//
	// 预留必须**持有到建行提交之后**才释放（defer 在函数出口）：先释放再落行
	// 会留下「行还没建、额度已还」的窗口 ⇒ 并发导入可超放。释放后该目录的
	// 用量由 DB 里的存活行承担，与在途预留口径一致（提交前双计，保守方向）。
	var releaseDirQuota func()
	if matched != nil && matched.QuotaBytes > 0 {
		releaseDirQuota, err = s.reserveDirectoryQuota(ctx, matched.Root, matched.QuotaBytes, info.Size())
		if err != nil {
			return nil, err
		}
	}
	if releaseDirQuota != nil {
		defer releaseDirQuota()
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
	// ⚠️ rc 必须判空：存储实现返回 (nil, nil)（读不到对象）时，HashReader(nil)
	// 会 panic ⇒ 整个导入通道被打挂。读不到就当没有哈希（不参与秒传）。
	fileHash := ""
	if rc, _, rerr := s.storage.GetFileReader(ctx, rel); rerr == nil && rc != nil {
		fileHash, _ = utils.HashReader(rc)
		_ = rc.Close()
	}
	channel := "local_import"
	// DirRoot：命中目录策略时写**解析后的策略 root**（用量归因 + 运维可定位）；
	// 未命中任何策略 ⇒ 空串（该行不计入任何目录的用量）。
	dirRoot := ""
	internalOnly := false
	if matched != nil {
		dirRoot = matched.Root
		// core#233：命中「仅站内」策略 ⇒ 该分享**定型**为仅站内
		//（不出公网取件码 + 取件强制登录）。建行时定型，与 deny 同款口径。
		internalOnly = matched.IsInternalOnly()
	}
	share, serr := s.CreateShare(ctx, &ShareFileReq{
		Channel:      channel,
		FilePath:     rel,
		Size:         written,
		Text:         name,
		DirRoot:      dirRoot,
		InternalOnly: internalOnly,
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
		// 曾永不被回收**（ReconcileOrphans 跳过远端；评审 D03 前 presign 孤儿扫描
		// 只认 up_ 前缀 ⇒ 本地导入的 UUID 名漏扫）⇒ 永久占空间。⇒ 失败即删副本。
		// （D03 后远端扫描已扩到 uploads/ 全量可兜底，但即时删除仍是最短泄漏窗口。）
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
