// 秒传/去重的物理对象保护（2026-10-12，D1=A 同用户内去重）。
//
// CreateShare 的去重钩子（service.go）让多个分享共享同一物理文件；本文件提供
// 共享对象的删除保护——任何删除分享物理对象的路径都必须先确认无其他分享引用
// （FileCodeRepository.CountFilePathReferences，含软删行：回收站行的物理文件
// 留存供恢复），查不清时跳过删除（宁泄漏勿误删，孤儿由 janitor 对账兜底）。
package share

import (
	"context"

	"github.com/pigeonbox/core/pkg/logger"
)

// deletePhysicalIfUnreferenced 物理删除保护：无其他分享引用 filePath 时才删。
// excludeFileCodeID=本次要失效的分享自身 ID（其行随本次操作一并失效，不计引用）。
func (s *Service) deletePhysicalIfUnreferenced(ctx context.Context, filePath string, excludeFileCodeID uint) {
	if s.storage == nil || filePath == "" {
		return
	}
	n, err := s.fileCodeRepo.CountFilePathReferences(ctx, filePath, excludeFileCodeID)
	if err != nil {
		logger.CtxWarn(ctx, "physical delete skipped: reference count failed (leak, janitor reconciles)",
			"path", filePath, "err", err)
		return
	}
	if n > 0 {
		return // 仍被其他分享引用（秒传共享对象），保留物理文件
	}
	if err := s.storage.DeleteFile(ctx, filePath); err != nil {
		logger.CtxWarn(ctx, "delete physical file failed", "path", filePath, "err", err)
	}
}
