// blobstate.go —— 分片会话对文件生命周期账本的接入（core#172 第三片·拆分项 3）。
//
// # 状态流转接点（与完成链各步骤一一对应）
//
//	MergeUpload 成功 + 合并结果可读
//	            → EnsureUploadBlob(committed)   字节已落盘（最终 object_key 在
//	                                             此刻才生成——分片先落 staging，
//	                                             不是最终对象的写入，故 chunk
//	                                             无 pending/uploading 段可观察）
//	魔数/大小复检失败（对象已 DeleteMergedFile）
//	            → Advance(aborted)              会话内容死亡（行不存在 noop）
//	ShareFile → CreateShare → attachBlobRef（首片 EnsureBlobWithRef）
//	            → active                       完整链 Committed→Active 生产走通
//
// # 取舍
//
//   - 为什么入口是 committed 不是 pending/uploading：最终 object_key 由
//     MergeUpload 内部的 UploadObjectRelPath（UUID）生成，merge 之前服务端
//     根本不知道 key；staging 分片（chunks/<upload_id>/）不是最终对象，给它
//     落账会污染路径口径的回收守卫。字节全部落盘才拿到 key ⇒ committed 就是
//     本路径最早的诚实入口（与 blob_upload.go 头注的分工说明一致）。
//   - best-effort（同 presign/blobstate.go 与首片 blobref.go 口径）：账本失败
//     仅告警不阻断完成主链；泄漏方向由既有路径口径与 janitor 对账兜底。
//   - 行不存在 ⇒ Advance noop（复检失败但从未落账的会话不建行）。
package chunk

import (
	"context"

	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/repo/db/model"
)

// BlobEnsureCommitted 合并成功后落账（committed 入口 = 字节已落盘）。
// 行不存在则建，已存在则回填 size/hash / 幂等 noop（重复 complete 不重复计数）。
func (s *Service) BlobEnsureCommitted(ctx context.Context, uploadID, objectKey string, size int64, fileHash string) {
	if s.blobRepo == nil || objectKey == "" {
		return
	}
	if _, _, err := s.blobRepo.EnsureUploadBlob(ctx, objectKey, size, fileHash, model.BlobCommitted); err != nil {
		logger.CtxWarn(ctx, "blob committed ledger failed (complete proceeds; path-based reclaim still guards)",
			"upload_id", uploadID, "path", objectKey, "err", err)
	}
}

// BlobAbort 合并后复检失败、对象已删：账本转 aborted（旁支终态）。
// 行不存在 = 从未落账 ⇒ noop，不建行。
func (s *Service) BlobAbort(ctx context.Context, uploadID, objectKey string) {
	if s.blobRepo == nil || objectKey == "" {
		return
	}
	if _, _, err := s.blobRepo.AdvanceUploadBlob(ctx, objectKey, model.BlobAborted); err != nil {
		logger.CtxWarn(ctx, "blob abort ledger failed (object already deleted; ledger may lag)",
			"upload_id", uploadID, "path", objectKey, "err", err)
	}
}
