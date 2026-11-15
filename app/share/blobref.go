// blobref.go 分享创建/硬删对文件生命周期账本（blobs/file_references）的接入
// （core#172 首片·写路径）。
//
// # 取舍（PR 说明同源）
//
//   - **包装现有写，不搞大迁移**：file_codes/file_code_files 仍是分享与文件
//     清单的唯一真相源；本文件只在其后**并行**补账本行（适配层），
//     不改任何既有表结构、不回填存量（回填/懒迁移口径见 issue 拆分清单）。
//   - **best-effort（失败仅告警，不阻断分享创建）**：账本是新增面，双写失败
//     的后果是「账本缺行 ⇒ 该对象不走新回收闸」——物理对象仍由既有
//     deletePhysicalIfUnreferenced（CountFilePathReferences 路径口径）与
//     janitor 孤儿对账兜底，**泄漏方向、不误删**。反过来若因账本失败拒绝
//     分享创建，是把新面的可用性风险倒灌进主链，不可取。
//   - **软删不释放**：释放只挂在硬删（HardDeleteUserShare）；软删/恢复不碰
//     账本（回收站行仍占对象，恢复后对象必须在——与 CountFilePathReferences
//     含软删行的既有口径一致）。
//   - 匿名占位通道（app/anonymous 直接建行，presign 取件码绑定面）与分片/
//     presign 会话的 Pending→Uploading 段不接首片，见 issue 拆分清单。
package share

import (
	"context"

	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/repo/db/model"
)

// attachBlobRef 分享创建后的账本双写（best-effort）：
// objectKey 空（文本分享/占位）直接跳过；失败仅告警（见文件头取舍）。
func (s *Service) attachBlobRef(ctx context.Context, fc *model.FileCode) {
	if s.blobRepo == nil || fc == nil {
		return
	}
	key := fc.GetFilePath()
	if key == "" {
		return // 文本分享无物理对象
	}
	if _, err := s.blobRepo.EnsureBlobWithRef(ctx, key, fc.Size, fc.FileHash, fc.ID); err != nil {
		logger.CtxWarn(ctx, "blob ref attach failed (share still created; path-based reclaim still guards)",
			"code", fc.Code, "path", key, "err", err)
	}
}

// attachBlobRefs 多文件分享：逐子文件路径补账本（首个子行与主行同路径，
// EnsureBlobWithRef 幂等 ⇒ 不会产生重复行/重复引用）。
func (s *Service) attachBlobRefs(ctx context.Context, fileCodeID uint, children []*model.FileCodeFile) {
	if s.blobRepo == nil {
		return
	}
	for _, c := range children {
		if c.FilePath == "" {
			continue
		}
		if _, err := s.blobRepo.EnsureBlobWithRef(ctx, c.FilePath, c.Size, c.FileHash, fileCodeID); err != nil {
			logger.CtxWarn(ctx, "blob ref attach failed (multi-file; path-based reclaim still guards)",
				"file_code_id", fileCodeID, "path", c.FilePath, "err", err)
		}
	}
}

// releaseBlobRef 分享**硬删**后的引用释放（best-effort，幂等）。
//
// 归零 ⇒ blob 进 trashed（宽限期起点）；未归零 ⇒ 保持 active（共享对象
// 负对照）。软删不调用本方法。失败仅告警：账本残留 active ⇒ 对象不进
// 新回收闸 ⇒ 泄漏方向，由既有路径口径与 janitor 对账兜底。
func (s *Service) releaseBlobRef(ctx context.Context, fileCodeID uint) {
	if s.blobRepo == nil || fileCodeID == 0 {
		return
	}
	if n, err := s.blobRepo.ReleaseByShare(ctx, fileCodeID); err != nil {
		logger.CtxWarn(ctx, "blob ref release failed (hard delete already done; ledger row may linger)",
			"file_code_id", fileCodeID, "err", err)
	} else if n > 0 {
		logger.CtxInfo(ctx, "blob refs released on hard delete",
			"file_code_id", fileCodeID, "released", n)
	}
}
