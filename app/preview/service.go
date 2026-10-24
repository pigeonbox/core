// Package preview 预览域服务：取件校验与预览生成/持久化的业务编排。
//
// gen/handler/preview 是 HTTP 适配层（参数提取 + 错误→状态码映射），
// 闸门/过期/封禁/防爆破/密码/存储读取/预览生成/落库的业务事实全部收口于此。
package preview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/oekit/concurrency/syncx"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/gate"
	previewService "github.com/pigeonbox/core/preview"
	"github.com/pigeonbox/core/repo/db/dao"
	dao_preview "github.com/pigeonbox/core/repo/db/dao_preview"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/pigeonbox/core/storage"
	"gorm.io/gorm"
)

// 业务错误（HTTP 适配层据此映射状态码与文案，不泄露内部细节）。
var (
	// ErrShareNotFound 分享不存在/已过期/不可见（统一 404 语义）
	ErrShareNotFound = errors.New("share not found or expired")
	// ErrShareBlocked 分享被封禁或待审（走 errcode 管控语义）
	ErrShareBlocked = errors.New("share blocked")
	// ErrPasswordRequired 密码保护分享未通过密码校验（含防爆破锁定态）
	ErrPasswordRequired = errors.New("password required")
	// ErrLocked 密码错误次数过多，临时锁定
	ErrLocked = errors.New("too many attempts, temporarily locked")
	// ErrShareNotReady 分享未完成上传（占位分享；与 select/download 同码同文案）
	ErrShareNotReady = errors.New("share not uploaded yet")

	// ErrPickupExhausted 取件次数已用完（2026-10-24：此前次数耗尽混作「已过期」
	// ⇒ 预览面返回 404「分享不存在或已过期」，与文档承诺的 10003 对不上）。
	ErrPickupExhausted = errors.New("pickup count exhausted")
	// ErrPreviewUnavailable 预览不可用（生成失败/存储不可达）
	ErrPreviewUnavailable = errors.New("preview unavailable")
	// ErrPreviewBusy 生成并发闸满（防 CPU/内存耗尽：图片解码与 ffmpeg 均为重活，
	// 攻击者跨 IP 并发预览未缓存的大图/视频可打满 CPU——限流按 IP 计数堵不住分布式）
	ErrPreviewBusy = errors.New("preview generation busy")
)

// LockedError 防爆破锁定态（携带剩余锁定秒数，供文案）
type LockedError struct{ Remain int }

func (e *LockedError) Error() string { return fmt.Sprintf("locked for %ds", e.Remain) }

// previewGenGate 预览生成并发闸（2026-10-08 加固）：生成是同步重活（图片
// 全量解码/ffmpeg 子进程），用满配额即忙拒——try-acquire 不排队，避免请求
// 堆积把内存换成另一种 DoS。
//
// 收编 oekit/concurrency/syncx.Gate：上游 Gate 的 TryAcquire 正是「满额即拒、
// 不排队」的形态，且归还名额幂等（此前自维护的 chan struct{} 归还与占用分离，
// 重复归还会把名额多还一个）。
var previewGenGate = syncx.NewGate(2)

// Service 预览域服务。存储实例由 bootstrap 注入；nil 时预览生成不可用
// （回归 2026-10-03：此前硬编码 data/uploads 路径基，与统一存储不一致）。
type Service struct {
	storageSvc   storage.StorageInterface
	fileCodeRepo *dao.FileCodeRepository            // 取件校验读分享记录（此前方法体内联现建）
	previewRepo  *dao_preview.FilePreviewRepository // 预览读写
}

// NewService 构造预览域服务。
func NewService(gormDB *gorm.DB, st storage.StorageInterface) *Service {
	return &Service{
		storageSvc:   st,
		fileCodeRepo: dao.NewFileCodeRepository(gormDB),
		previewRepo:  dao_preview.NewFilePreviewRepository(gormDB),
	}
}

// GetReq 预览获取请求。HTTP 关注点（取件码/密码/客户端 IP/登录态）由
// 适配层提取后传入；锁定、校验、生成、落库均为域内事实。
type GetReq struct {
	Code     string
	UserID   *uint  // 登录态（nil = 匿名）
	ClientIP string // 可信代理解析后的取件方 IP（防爆破锁定键）
	Password string // 密码保护分享的取件密码
}

// GetOrCreate 取预览：无则生成并持久化（get-or-create）。
func (s *Service) GetOrCreate(ctx context.Context, req GetReq) (*model.FilePreview, error) {
	// 下载闸门 + 分享管控（回归 P0：此前裸查 DB，blocked/待审/过期/密码/登录闸门
	// 全部旁路，违规内容可被匿名预览提取全文）
	if err := gate.CheckDownloadLogin(req.UserID); err != nil {
		return nil, err
	}

	// 获取文件信息（**分享码或 6 位取件码**）：走统一解析器（dao.ResolveByCode），
	// 与 select/metadata 口径一致（此前本域只查 code 列 ⇒ 取件码在此 404，
	// 而分享页能打开 ⇒ 同一输入两端点结论相反）。
	fileCode, err := s.fileCodeRepo.ResolveByCode(ctx, req.Code, conf.CodeFoldEnabledOrDefault())
	if err != nil {
		return nil, ErrShareNotFound
	}
	// 统一取件闸门（pkg/gate.CheckPickup）：过期/管控/密码/防爆破锁定与
	// select/download/匿名取件面同规格、同 (IP, code) 失败计数（此前本域
	// 独立 scope "preview" 且用 FormatLockKey 不折叠取件码大小写，存在
	// 变体绕过锁定漏洞，随收口一并修复）。
	// 占位分享（文件未上传）：与 select/download 面同口径 —— 20001
	// 「分享未完成上传」（此前本域落 500/预览不可用 ⇒ 口径不一致）。
	if model.IsNotReady(fileCode) {
		return nil, ErrShareNotReady
	}
	// 取件次数已用完：预览也是「拿内容」的面 ⇒ 与 select/download 同口径拒绝
	// （预览不消耗次数，但次数耗尽后不应再能看到内容摘要）。
	if fileCode.IsCountExhausted() {
		return nil, ErrPickupExhausted
	}
	decision := gate.CheckPickup(ctx, gate.PickupState{
		Expired:       fileCode.IsExpired(),
		Blocked:       fileCode.IsBlockedShare(),
		BlockedStatus: fileCode.Status,
		RequireAuth:   fileCode.RequireAuth,
		PasswordHash:  fileCode.PasswordHash,
	}, gate.PickupCheck{
		ClientIP: req.ClientIP,
		// 失败记账用**规范分享码**（6 位取件码解析后）⇒ 与 select/下载/匿名面
		// 同一把钥匙，爆破预算不翻倍（2026-10-23 审计）。
		Code:                 fileCode.Code,
		Password:             req.Password,
		MissingCountsFailure: true, // 保持本域历史规格：空密码按失败记账
	})
	switch decision.Verdict {
	case gate.PickupUnavailable:
		return nil, ErrShareNotFound
	case gate.PickupBlocked:
		return nil, ErrShareBlocked
	case gate.PickupLocked:
		return nil, &LockedError{Remain: decision.LockRemain}
	case gate.PickupPasswordMissing, gate.PickupPasswordWrong:
		return nil, ErrPasswordRequired
	}

	// 取预览；不存在则生成
	preview, err := s.previewRepo.GetByFileCodeID(ctx, fileCode.ID)
	if err == nil {
		return preview, nil
	}
	// 并发闸：只在真实生成路径占用（缓存命中零开销）
	release, ok := previewGenGate.TryAcquire()
	if !ok {
		return nil, ErrPreviewBusy
	}
	defer release()
	preview, err = s.generatePreview(ctx, fileCode)
	if err != nil {
		return nil, ErrPreviewUnavailable
	}
	return preview, nil
}

// generatePreview 生成预览。经统一存储实例读取文件（回归：此前硬编码
// data/uploads 路径基且未拼 UUIDFileName，与存储布局不符）。
func (s *Service) generatePreview(ctx context.Context, fileCode *model.FileCode) (*model.FilePreview, error) {
	filePath := fileCode.GetFilePath()
	if filePath == "" {
		return nil, fmt.Errorf("file path is empty")
	}
	// 预览大小上限（2026-10-08 加固）：Config.MaxFileSize 此前从未生效，任意大
	// 文件会先全量落临时盘才被生成器拒绝——磁盘耗尽 DoS 面。DB 有记录大小时
	// 预判直接拒绝（连对象都不打开）；记录缺失/为 0 时退化为流式上限截断。
	maxSize := previewService.GetService().MaxFileSize()
	if maxSize > 0 && fileCode.Size > maxSize {
		return nil, fmt.Errorf("file too large for preview: %d > %d", fileCode.Size, maxSize)
	}
	if s.storageSvc == nil {
		return nil, fmt.Errorf("storage not available")
	}
	rc, _, err := s.storageSvc.GetFileReader(ctx, filePath)
	if err != nil {
		return nil, fmt.Errorf("open file from storage: %w", err)
	}
	// GeneratePreview 以磁盘路径为输入，落临时文件后清理
	tmp, err := os.CreateTemp("", "pb-preview-*")
	if err != nil {
		_ = rc.Close()
		return nil, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	// 兜底截断：源大小未知（Size=0 老数据）时也不允许暂存超限内容
	var src io.Reader = rc
	if maxSize > 0 {
		src = io.LimitReader(rc, maxSize+1)
	}
	written, copyErr := io.Copy(tmp, src)
	_ = rc.Close()
	_ = tmp.Close()
	if copyErr != nil {
		return nil, fmt.Errorf("stage file: %w", copyErr)
	}
	if maxSize > 0 && written > maxSize {
		return nil, fmt.Errorf("file too large for preview: %d > %d", written, maxSize)
	}
	return s.generatePreviewFromPath(ctx, fileCode, tmpPath)
}

func (s *Service) generatePreviewFromPath(ctx context.Context, fileCode *model.FileCode, filePath string) (*model.FilePreview, error) {
	// 判断文件类型
	ext := fileCode.Suffix
	if ext == "" {
		// 从UUID文件名提取扩展名
		if fileCode.UUIDFileName != "" {
			for i := len(fileCode.UUIDFileName) - 1; i >= 0; i-- {
				if fileCode.UUIDFileName[i] == '.' {
					ext = fileCode.UUIDFileName[i:]
					break
				}
			}
		}
		// 如果还是没有，从Text字段（原始文件名）提取
		if ext == "" && fileCode.Text != "" {
			for i := len(fileCode.Text) - 1; i >= 0; i-- {
				if fileCode.Text[i] == '.' {
					ext = fileCode.Text[i:]
					break
				}
			}
		}
	}

	// 获取预览引擎
	svc := previewService.GetService()
	if svc == nil {
		return nil, fmt.Errorf("preview service not available")
	}

	// 生成预览（filePath 由调用方经统一存储暂存提供）
	previewData, err := svc.GeneratePreview(ctx, filePath, ext)
	if err != nil {
		return nil, fmt.Errorf("failed to generate preview: %w", err)
	}

	// 保存预览信息到数据库
	preview := &model.FilePreview{
		FileCodeID:  fileCode.ID,
		PreviewType: string(previewData.Type),
		Thumbnail:   previewData.Thumbnail,
		PreviewURL:  previewData.PreviewURL,
		Width:       previewData.Width,
		Height:      previewData.Height,
		Duration:    previewData.Duration,
		PageCount:   previewData.PageCount,
		TextContent: previewData.TextContent,
		MimeType:    previewData.MimeType,
		FileSize:    previewData.FileSize,
	}

	if err := s.previewRepo.Create(ctx, preview); err != nil {
		return nil, fmt.Errorf("failed to save preview: %w", err)
	}

	return preview, nil
}
