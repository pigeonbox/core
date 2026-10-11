// maintenance.go 维护清理簇：过期文件清理/未完成上传清理/数据库优化。
package admin

import (
	"context"
	"fmt"
	"time"
)

// CleanupExpiredFiles 清理过期文件
func (s *Service) CleanupExpiredFiles(ctx context.Context) (int, error) {
	// 获取过期文件
	expiredFiles, err := s.fileCodeRepo.GetExpiredFiles(ctx)
	if err != nil {
		return 0, err
	}

	// 删除过期文件
	deletedCount, err := s.fileCodeRepo.DeleteExpiredFiles(ctx, expiredFiles)
	if err != nil {
		return 0, err
	}

	s.logAdminOperation(ctx, "maintenance.clean_expired_files",
		fmt.Sprintf("cleaned %d expired files", deletedCount), err == nil)
	return deletedCount, nil
}

// CleanupIncompleteUploads 清理未完成的上传
func (s *Service) CleanupIncompleteUploads(ctx context.Context, olderThanHours int) (int, error) {
	// 获取未完成的上传
	incompleteUploads, err := s.chunkRepo.GetIncompleteUploads(ctx, time.Duration(olderThanHours)*time.Hour)
	if err != nil {
		return 0, err
	}

	uploadIDs := make([]string, len(incompleteUploads))
	for i, upload := range incompleteUploads {
		uploadIDs[i] = upload.UploadID
	}

	// 删除未完成的上传记录
	deletedCount, err := s.chunkRepo.DeleteChunksByUploadIDs(ctx, uploadIDs)
	if err != nil {
		return 0, err
	}

	s.logAdminOperation(ctx, "maintenance.clean_incomplete_uploads",
		fmt.Sprintf("cleaned %d incomplete uploads (older than %dh)", deletedCount, olderThanHours), err == nil)
	return deletedCount, nil
}

// ==================== 维护工具 API ====================

// CleanExpiredFiles 清理过期文件（DB 记录 + 物理文件）。
// 物理删除失败不阻断 DB 删除（记日志）；DB 删除失败不阻断（下轮重试）。
func (s *Service) CleanExpiredFiles(ctx context.Context) (int64, int64, error) {
	expiredFiles, err := s.fileCodeRepo.GetExpiredFiles(ctx)
	if err != nil {
		return 0, 0, err
	}

	freedSpace := int64(0)
	childRepo := s.fileFileRepo
	for _, file := range expiredFiles {
		// 多文件分享子文件一并清理（P0 多文件；失败不阻断 DB 删除）；
		// 秒传共享对象保护（2026-10-12）：删前查引用，被其他分享共享时保留物理文件
		if s.storage != nil {
			if children, cerr := childRepo.ListByFileCodeID(ctx, file.ID); cerr == nil {
				for _, c := range children {
					if c.FilePath == "" {
						continue
					}
					s.deletePhysicalIfUnreferenced(ctx, c.FilePath, file.ID)
				}
				if len(children) > 0 {
					_ = childRepo.SoftDeleteByFileCodeIDs(ctx, []uint{file.ID})
				}
			}
		}
		// 删物理文件（失败不阻断 DB 删除）
		if s.storage != nil && file.FilePath != "" {
			s.deletePhysicalIfUnreferenced(ctx, file.GetFilePath(), file.ID)
		}
		freedSpace += file.Size
	}

	count, err := s.fileCodeRepo.DeleteExpiredFiles(ctx, expiredFiles)
	if err != nil {
		return 0, 0, err
	}
	return int64(count), freedSpace, nil
}

// CleanTempFiles 清理临时文件
func (s *Service) CleanTempFiles(ctx context.Context) (int64, int64, error) {
	// 获取24小时前未完成的会话
	incompleteUploads, err := s.chunkRepo.GetIncompleteUploads(ctx, 24*time.Hour)
	if err != nil {
		return 0, 0, err
	}

	uploadIDs := make([]string, 0, len(incompleteUploads))
	for _, upload := range incompleteUploads {
		uploadIDs = append(uploadIDs, upload.UploadID)
	}

	// 删除未完成的上传记录
	deletedCount, err := s.chunkRepo.DeleteChunksByUploadIDs(ctx, uploadIDs)
	if err != nil {
		return 0, 0, err
	}

	return int64(deletedCount), 0, nil
}

// OptimizeDatabase 数据库优化（维护工具「优化数据库」）。
// sqlite：VACUUM 回收空间 + ANALYZE 刷新统计（此前前端按钮调用的是
// 后端从未实现的 /admin/maintenance/optimize，恒 404——2026-10-06 补齐）；
// mysql：ANALYZE 核心表；postgres：VACUUM ANALYZE；其他后端 no-op。
// 返回 (实际优化的驱动, 执行说明)。
func (s *Service) OptimizeDatabase(ctx context.Context) (string, string, error) {
	// VACUUM/ANALYZE 等方言细节下沉 DAO（架构规则 3：app 层不裸引 gorm）
	return s.sysConfigRepo.Optimize(ctx)
}
