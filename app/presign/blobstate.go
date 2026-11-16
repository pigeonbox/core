// blobstate.go —— presign 会话对文件生命周期账本的接入（core#172 第三片·拆分项 3）。
//
// # 状态流转接点（与 presign.go 各步骤一一对应）
//
//	Init        → EnsureUploadBlob(pending)   会话已建、写未开始（对象已定位）
//	UploadDirect → Advance(uploading)          PUT 开始写
//	            → Advance(committed)          字节写完（大小复核通过后）
//	Complete    → Advance(committed)          s3 方案的兜底推进（服务端不见流量，
//	                                          以 HeadObject 存在性核实为据，沿
//	                                          pending→uploading→committed 逐跳走）
//	            → Advance(aborted)            过期拒收 / 内容复检失败（对象已删）
//	Abort       → Advance(aborted)            会话显式中止
//
// 之后分享创建（createShareRecord → share.CreateShare → attachBlobRef）沿首片的
// EnsureBlobWithRef 把 committed 推进到 active 并挂引用 —— 完整链
// Pending→Uploading→Committed→Active 在生产走通。
//
// # 取舍
//
//   - best-effort（同 blobref.go 口径）：账本失败仅告警，不阻断上传/完成主链；
//     泄漏方向（缺行/状态停在前段）由既有路径口径与 janitor 对账兜底。
//   - 不在写失败分支 abort：PUT 写失败会话仍可重试（meta 未标 complete），
//     状态停 uploading 时重放 PUT 是 noop；abort 只挂会话确实死亡的点
//     （Abort/过期/内容拒绝）。行不存在 ⇒ DAO noop，不建行。
package presign

import (
	"context"

	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/repo/db/model"
)

// blobEnsurePending 会话落账（pending 入口）：行不存在则建，已存在幂等。
func (s *Service) blobEnsurePending(ctx context.Context, meta *InitMeta) {
	if s.blobRepo == nil || meta == nil || meta.ObjectKey == "" {
		return
	}
	if _, _, err := s.blobRepo.EnsureUploadBlob(ctx, meta.ObjectKey, meta.FileSize, meta.FileHash, model.BlobPending); err != nil {
		logger.CtxWarn(ctx, "blob pending ledger failed (upload proceeds; path-based reclaim still guards)",
			"upload_id", meta.UploadID, "path", meta.ObjectKey, "err", err)
	}
}

// blobAdvance 推进会话账本到 target（uploading/committed/aborted）；
// 行不存在 = 未落账，DAO noop。
func (s *Service) blobAdvance(ctx context.Context, objectKey, target string) {
	if s.blobRepo == nil || objectKey == "" {
		return
	}
	if _, _, err := s.blobRepo.AdvanceUploadBlob(ctx, objectKey, target); err != nil {
		logger.CtxWarn(ctx, "blob upload-state advance failed (upload proceeds; ledger may lag)",
			"path", objectKey, "target", target, "err", err)
	}
}
