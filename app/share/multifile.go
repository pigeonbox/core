package share

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pigeonbox/core/app/moderation"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/pkg/metrics"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/pigeonbox/core/storage"
	"gorm.io/gorm"
)

// StoredFileEntry 已落存储的文件项（多文件分享创建入参）。
// 结构定义已下沉 storage.StoredFileEntry（中立载体，request 等域零 share 依赖）；
// 本别名保留既有引用路径，新代码可直接使用 storage.StoredFileEntry。
type StoredFileEntry = storage.StoredFileEntry

// MultiShareReq 多文件分享创建请求。
type MultiShareReq struct {
	Entries      []StoredFileEntry
	ExpiredAt    *time.Time
	ExpiredCount int
	RequireAuth  bool
	PasswordHash string
	UserID       *uint
	UploadType   string
	OwnerIP      string
	Channel      string // direct/chunk/presign（metrics/审核 meta 用）
	Encrypted    bool   // E2E：所有子文件均为客户端密文（P1）
	WrappedKey   string // 口令型 E2E 信封密钥（与单文件同语义：非空⇒铸 6 位取件码）
	CustomCode   string // 自定义取件码（P3；登录用户专属，handler 把关）
	Note         string // 文件备注（可空，取件页展示；2026-10-12 #489）
}

// ShareFileItem 分享内单文件视图（取件端/管理端通用）。
type ShareFileItem struct {
	ID       uint   `json:"id"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	FileHash string `json:"file_hash"`
}

// maxMultiFiles 单个分享的文件数上限（防滥用；upload.max_files_per_share 可调）
const maxMultiFiles = 100

// CreateMultiFileShare 创建多文件分享（1 个取件码 ↔ N 个文件）。
//
// 与单文件 CreateShare 的差异：
//   - 主表 Size = 各文件之和；主表 FilePath/UUIDFileName/FileHash 与首个文件同步
//     （管理端列表/秒传/统计读主表，零改动可用）
//   - 子表每文件一行，取件端经 ListShareFiles 拉取
//   - 配额按总大小一次扣检；审核逐文件判定（任一 reject 整单拒绝，pending 整单待审）
//
// 失败时不回滚物理文件——调用方（handler）负责在 err != nil 时清理已落盘文件。
func (s *Service) CreateMultiFileShare(ctx context.Context, req *MultiShareReq) (*ShareResp, error) {
	if len(req.Entries) == 0 {
		return nil, errors.New("至少需要一个文件")
	}
	if len(req.Entries) > maxMultiFiles {
		return nil, fmt.Errorf("单次分享文件数超过上限（%d 个）", maxMultiFiles)
	}

	var totalSize int64
	for _, e := range req.Entries {
		if e.RelPath == "" {
			return nil, errors.New("文件路径为空")
		}
		totalSize += e.Size
	}

	// 单用户单次上传总大小上限 + 存储配额（合计一次校验/扣检）
	if err := s.checkUploadCaps(ctx, req.UserID, totalSize); err != nil {
		return nil, err
	}

	// 内容审核：逐文件判定，任一命中即整单处置（reject 拒绝 / pending 待审）
	filePending := false
	if s.moderator != nil {
		for _, e := range req.Entries {
			meta := moderation.UploadMeta{
				FileName:    e.FileName,
				Size:        e.Size,
				OwnerIP:     req.OwnerIP,
				UserID:      req.UserID,
				Channel:     req.Channel,
				StoragePath: e.RelPath, // 文件内容扫描（ClamAV）读取路径
			}
			p, err := s.inspectFile(ctx, meta)
			if err != nil {
				return nil, err
			}
			if p {
				filePending = true
			}
		}
	}

	first := req.Entries[0]
	fileCode, err := s.createWithCode(ctx, req.CustomCode, func(code string) *model.FileCode {
		return &model.FileCode{
			Code:         code,
			FilePath:     first.RelPath,
			UUIDFileName: uniqueNameOf(first.RelPath),
			Size:         totalSize,
			Text:         first.FileName, // 文件分享的 Text 存首个原始文件名（与单文件直传通道一致）
			ExpiredAt:    req.ExpiredAt,
			ExpiredCount: req.ExpiredCount,
			RequireAuth:  req.RequireAuth,
			PasswordHash: req.PasswordHash,
			UserID:       req.UserID,
			UploadType:   req.UploadType,
			OwnerIP:      req.OwnerIP,
			FileHash:     first.FileHash,
			Encrypted:    req.Encrypted,
			WrappedKey:   req.WrappedKey,
			Note:         req.Note,
		}
	})
	if err != nil {
		return nil, err
	}

	// 子表批量写入；失败则回滚主表（分享未发布，物理文件交 handler 清理）
	children := make([]*model.FileCodeFile, 0, len(req.Entries))
	for i, e := range req.Entries {
		children = append(children, &model.FileCodeFile{
			FileCodeID:   fileCode.ID,
			FileName:     e.FileName,
			UUIDFileName: uniqueNameOf(e.RelPath),
			FilePath:     e.RelPath,
			Size:         e.Size,
			FileHash:     e.FileHash,
			SortOrder:    i,
		})
	}
	if err := s.fileFileRepo.CreateBatch(ctx, children); err != nil {
		_ = s.fileCodeRepo.Delete(ctx, fileCode.ID)
		return nil, fmt.Errorf("写入分享文件列表失败: %w", err)
	}

	// 用户统计：次数 +1，存储按合计
	s.bumpUserStats(req.UserID, totalSize)

	// pending 策略（fail-closed 同单文件通道）
	if filePending {
		if _, err := s.applyPending(ctx, fileCode.ID, fileCode.Code, "file flagged by moderator", req.OwnerIP); err != nil {
			return nil, err
		}
	}

	channel := req.Channel
	if channel == "" {
		channel = req.UploadType
	}
	metrics.RecordUploadBytes(channel, totalSize)
	metrics.RecordShareCreated(req.UploadType)
	// P2P 联邦公告（未启用为 no-op）
	s.federationCreated(fileCode.Code, fileCode.ExpiredAt)

	resp := s.modelToResp(ctx, fileCode)
	// 6 位取件码铸造（2026-10-07，与 CreateShare 同策略：非致命）
	if s.pickupMinter != nil {
		displayName := fmt.Sprintf("%d files", len(req.Entries))
		if len(req.Entries) == 1 {
			displayName = req.Entries[0].FileName
		}
		if pickup, err := s.pickupMinter.MintForShare(ctx, fileCode.Code, displayName, totalSize, req.RequireAuth, req.ExpiredAt); err != nil {
			logger.CtxWarn(ctx, "mint pickup code failed (non-fatal, multi)",
				"code", fileCode.Code, "err", err)
		} else {
			resp.PickupCode = pickup
		}
	}
	return resp, nil
}

// ListShareFiles 取分享的文件列表（取件端展示/下载入口）。
// 子表为唯一真相源：每个文件分享都有对应子表行（启动期数据迁移已补齐存量）。
func (s *Service) ListShareFiles(ctx context.Context, code string) ([]*ShareFileItem, error) {
	fc, err := s.fileCodeRepo.ResolveByCode(ctx, code, conf.CodeFoldEnabledOrDefault())
	if err != nil {
		return nil, err
	}
	return s.shareFileItems(ctx, fc)
}

// shareFileItems 内部：FileCode → 文件项列表（子表为唯一真相源）
//
// 2026-10-26 兼容层退役：旧单文件分享无子表行时曾在主表合成「虚拟行」（id=0），
// 取件端/打包下载各自再判一次「id==0 ⇒ 走主表路径」。该形态已被启动期数据迁移
// 补齐为真实子表行（见 bootstrap/datamigrate.go），虚拟行不再存在。
func (s *Service) shareFileItems(ctx context.Context, fc *model.FileCode) ([]*ShareFileItem, error) {
	children, err := s.fileFileRepo.ListByFileCodeID(ctx, fc.ID)
	if err != nil {
		return nil, err
	}
	items := make([]*ShareFileItem, 0, len(children))
	for _, c := range children {
		items = append(items, &ShareFileItem{
			ID:       c.ID,
			Name:     c.DisplayName(),
			Size:     c.Size,
			FileHash: c.FileHash,
		})
	}
	return items, nil
}

// GetShareChild 取分享内的单个子文件（校验归属，防跨分享枚举文件 ID）。
func (s *Service) GetShareChild(ctx context.Context, code string, fileID uint) (*model.FileCode, *model.FileCodeFile, error) {
	fc, err := s.fileCodeRepo.ResolveByCode(ctx, code, conf.CodeFoldEnabledOrDefault())
	if err != nil {
		return nil, nil, err
	}
	if fileID == 0 {
		// id=0 是旧「虚拟行」编号，已随兼容层退役（前端单文件分享不传 file 参数）
		return nil, nil, errors.New("文件不存在")
	}
	child, err := s.fileFileRepo.GetByID(ctx, fileID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, errors.New("文件不存在")
		}
		return nil, nil, err
	}
	if child.FileCodeID != fc.ID {
		return nil, nil, errors.New("文件不属于该分享")
	}
	return fc, child, nil
}

// deletePhysicalChildren 删除分享的全部子文件（物理 + 软删子行）。
// 物理删除失败记日志不阻断（与既有删除语义一致，孤儿由 janitor 对账）。
func (s *Service) deletePhysicalChildren(ctx context.Context, fileCodeID uint) {
	children, err := s.fileFileRepo.ListByFileCodeID(ctx, fileCodeID)
	if err != nil {
		logger.CtxWarn(ctx, "list children for delete failed", "file_code_id", fileCodeID, "err", err)
		return
	}
	if s.storage == nil {
		return
	}
	for _, c := range children {
		if c.FilePath == "" {
			continue
		}
		// 秒传共享对象保护（2026-10-12）：删前查引用，多分享共享同一物理文件时保留
		s.deletePhysicalIfUnreferenced(ctx, c.FilePath, fileCodeID)
	}
	if err := s.fileFileRepo.SoftDeleteByFileCodeIDs(ctx, []uint{fileCodeID}); err != nil {
		logger.CtxWarn(ctx, "soft delete child rows failed", "file_code_id", fileCodeID, "err", err)
	}
}

// uniqueNameOf 相对路径末段（唯一文件名）
func uniqueNameOf(relPath string) string {
	for i := len(relPath) - 1; i >= 0; i-- {
		if relPath[i] == '/' {
			return relPath[i+1:]
		}
	}
	return relPath
}
