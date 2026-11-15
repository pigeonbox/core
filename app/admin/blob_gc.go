// blob_gc.go 文件生命周期账本的回收核心（core#172 首片）：
// 删除分享 → 引用计数递减 → 归零且过宽限 → 回收对象（接既有 janitor 面）。
//
// # 与既有清理面的关系（收编，不另起炉灶）
//
//   - ReconcileOrphans（路径扫盘差集，local-only）与本方法**并存**：它兜
//     「DB 全无记录的裸孤儿」，本方法兜「账本状态机推进」——硬删时同步删
//     对象失败（路径抓取失败/存储错误，既有注释所谓 janitor 兜底）的泄漏，
//     由本方法在宽限期后补刀，并把 blob 行推进到 purged 终态收敛账本。
//   - **双守卫才删**（缺一不删，宁泄漏勿误删）：
//     ① 账本存活引用 = 0（快照后并发重建引用的竞态由 CountActiveRefs 复核）；
//     ② 路径引用 = 0 —— 沿用既有 CountFilePathReferences 口径（含软删行、
//     含**无账本行的存量分享**：首片不做回填，存量分享只有路径没有引用行，
//     只看账本会把它们当成零引用误删）。
//   - 软删不进候选：软删不释放引用 ⇒ blob 停在 active ⇒ 根本不在
//     ListReclaimable 结果里（负对照在 share 层 blobref_test.go 与本文件
//     路径守卫测试双层覆盖）。
//   - 删对象失败 ⇒ 留在 purge 态下轮重试（ListReclaimable 含 purge）；
//     对象已不存在 ⇒ 视为已删直接推进 purged（幂等收敛）。
//
// 后端面：与 share 域同步删对象同口径，**不限 local**——对象键来自 DB
// 精确记录（非扫盘），且删除前有双守卫；远端 DeleteFile 走 opendal 同样可靠。
package admin

import (
	"context"
	"time"

	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/repo/db/model"
	"go.uber.org/zap"
)

// ReclaimBlobs 回收「引用归零且过宽限」的物理对象，推进账本到 purged。
// 返回本轮回收（purged）数量。
//
// grace 为 trashed→purge 的宽限期（传 0 = 立即可回收，测试用；生产传
// 24h，与 ReconcileOrphans 的 mtime 宽限同量级）。
func (j *Janitor) ReclaimBlobs(ctx context.Context, grace time.Duration) (int, error) {
	if j.blobRepo == nil || j.svc == nil {
		return 0, nil // 账本面未装配/存储未就绪：不推进（宁停勿误删）
	}
	candidates, err := j.blobRepo.ListReclaimable(ctx, time.Now().Add(-grace))
	if err != nil {
		return 0, err
	}
	purged := 0
	for _, b := range candidates {
		// 守卫①：账本存活引用（ListReclaimable 是快照，其间可能有新引用把
		// blob 复活成 active——复核不过则跳过，状态已变由下轮再看）。
		alive, err := j.blobRepo.CountActiveRefs(ctx, b.ID)
		if err != nil {
			logger.CtxWarn(ctx, "blob reclaim: ledger refcount failed (skip)",
				"blob_id", b.ID, "err", err)
			continue
		}
		if alive > 0 {
			continue // 负对照：引用未归零不回收
		}
		// 守卫②：路径引用（既有口径，含软删行与无账本行的存量分享）。
		// 查不清 ⇒ 跳过（与 deletePhysicalIfUnreferenced 同款：查不清不删）。
		if b.ObjectKey != "" && j.fileCodeRepo != nil {
			pathRefs, err := j.fileCodeRepo.CountFilePathReferences(ctx, b.ObjectKey, 0)
			if err != nil {
				logger.CtxWarn(ctx, "blob reclaim: path refcount failed (skip)",
					"path", b.ObjectKey, "err", err)
				continue
			}
			if pathRefs > 0 {
				continue // 仍被分享引用（含软删/存量），不回收
			}
		}
		// trashed → purge（CAS：快照后状态被并发改动则跳过）。
		if b.State == model.BlobTrashed {
			ok, err := j.blobRepo.TransitionBlob(ctx, b.ID, model.BlobTrashed, model.BlobPurge)
			if err != nil {
				logger.CtxWarn(ctx, "blob reclaim: trashed to purge failed (skip)",
					"blob_id", b.ID, "err", err)
				continue
			}
			if !ok {
				continue // 并发下状态已变
			}
		}
		// 删物理对象（purge 态必经；对象已不存在视为已删——幂等收敛）。
		if b.ObjectKey != "" && j.svc.FileExists(ctx, b.ObjectKey) {
			if err := j.svc.DeleteFile(ctx, b.ObjectKey); err != nil {
				logger.Warn("blob reclaim: delete object failed (stay in purge for retry)",
					zap.String("path", b.ObjectKey), zap.Uint("blob_id", b.ID), zap.Error(err))
				continue // 留 purge 态，下轮重试
			}
		}
		// purge → purged（终态收敛）。
		ok, err := j.blobRepo.TransitionBlob(ctx, b.ID, model.BlobPurge, model.BlobPurged)
		if err != nil || !ok {
			logger.CtxWarn(ctx, "blob reclaim: purge to purged failed",
				"blob_id", b.ID, "err", err)
			continue
		}
		purged++
	}
	if purged > 0 {
		logger.Info("blob reclaim done", zap.Int("purged", purged),
			zap.Int("candidates", len(candidates)))
	}
	return purged, nil
}
