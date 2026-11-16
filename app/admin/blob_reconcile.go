// blob_reconcile.go 跨存储后端引用对账扫描（core#172 尾项）：账本 ↔ 路径 ↔ 存储
// 三方只读对账，四类漂移**只报告不修**（宁泄漏勿误删——修复动作留后续 issue）。
//
// 与既有两个对账面的分工：
//   - ReconcileOrphans（janitor.go）：物理文件 vs DB 引用差集，local 后端**真删**；
//   - ReclaimBlobs（blob_gc.go）：账本状态机推进 + 补删对象，双守卫才删；
//   - 本扫描：**不删任何东西**，跨全部 14 种存储后端同口径报告四类漂移，
//     供告警面/人工处置（gauge pb_blob_reconcile_drift{kind}）。
//
// 四类漂移（Drift* 常量 = gauge kind 标签，两侧字符串必须一致）：
//
//	dangling_ledger        ① 账本 active 行、路径零引用（悬挂账本行）——active
//	                       按状态机应有 ≥1 条存活引用，零引用说明分享已硬删但
//	                       账本未进回收态 ⇒ 对象永久泄漏（回收只挑 trashed/purge）；
//	missing_ledger         ② 路径有引用、账本无行（存量漏账）——首片不回填的
//	                       存量分享只有路径没有 blobs 行；按**去重后的路径**计数
//	                       （秒传多分享共享同一物理文件只算一笔）；
//	active_object_missing  ③ 账本 active 但存储对象不存在——分享指向 404；
//	purged_object_present  ④ 账本 purged（终态）但存储对象仍存在——删对象的
//	                       成功判定后对象又出现（后端一致性异常/外力复活），占盘泄漏。
//
// 存储实况一律走 storage.StorageInterface.FileExists（14 后端同口径，不硬编码
// 本地）。已知局限：远端后端瞬时网络错误会被记成"不存在"⇒ 单轮可能高估③④，
// 下一轮自愈；本面只报告不修，误报代价仅为一条告警。
package admin

import (
	"context"
	"fmt"

	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/pkg/metrics"
	"github.com/pigeonbox/core/repo/db/model"
	"go.uber.org/zap"
)

// Drift* 四类漂移的 kind 标签值（与 pkg/metrics/blob.go SetBlobReconcileDrift
// 的标签清单同源；admin 扫描测试断言 gauge 标签值，两侧漂移即红）。
const (
	DriftDanglingLedger      = "dangling_ledger"
	DriftMissingLedger       = "missing_ledger"
	DriftActiveObjectMissing = "active_object_missing"
	DriftPurgedObjectPresent = "purged_object_present"
)

// ScanBlobDrift 只读扫描四类漂移，返回 kind → 计数（四键恒在，无漂移=0）。
//
// 成功时**整表覆盖**刷新 gauge pb_blob_reconcile_drift{kind} 并在有漂移时落
// 一条 Info 日志；任一步失败返回 error 且**不刷新 gauge**（宁留上轮读数，不拿
// 半份数据覆盖成假 0）。只报告不删不改——修复动作留后续 issue。
func (j *Janitor) ScanBlobDrift(ctx context.Context) (map[string]int, error) {
	counts := map[string]int{
		DriftDanglingLedger:      0,
		DriftMissingLedger:       0,
		DriftActiveObjectMissing: 0,
		DriftPurgedObjectPresent: 0,
	}
	if j.blobRepo == nil || j.fileCodeRepo == nil || j.fileFileRepo == nil {
		return nil, fmt.Errorf("账本/路径仓库未装配")
	}
	if j.svc == nil {
		return nil, fmt.Errorf("存储实例未就绪")
	}

	blobs, err := j.blobRepo.ListAllBlobs(ctx)
	if err != nil {
		return nil, err
	}

	// 路径引用集（判据②的"路径有引用"侧；与 ReconcileOrphans 同口径：
	// 主表 GetFilePath + 子表路径，均含软删行）。
	rows, err := j.fileCodeRepo.ListAllIncludingDeleted(ctx)
	if err != nil {
		return nil, err
	}
	referenced := make(map[string]bool, len(rows))
	for _, r := range rows {
		if p := r.GetFilePath(); p != "" {
			referenced[p] = true
		}
	}
	childPaths, err := j.fileFileRepo.ListAllPathsIncludingDeleted(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range childPaths {
		if p != "" {
			referenced[p] = true
		}
	}

	// 账本 object_key 集（判据②的"账本有行"侧）。
	blobKeys := make(map[string]bool, len(blobs))
	for _, b := range blobs {
		if b.ObjectKey != "" {
			blobKeys[b.ObjectKey] = true
		}
	}

	for _, b := range blobs {
		if b.ObjectKey == "" {
			continue // 空 key 无法对账（防御：唯一索引理论上不放行）
		}
		switch b.State {
		case model.BlobActive:
			// 判据①：active 行路径零引用 ⇒ 悬挂账本行（与回收守卫同用
			// CountFilePathReferences——含软删行、含子表，口径同源）。
			n, err := j.fileCodeRepo.CountFilePathReferences(ctx, b.ObjectKey, 0)
			if err != nil {
				return nil, err
			}
			if n == 0 {
				counts[DriftDanglingLedger]++
			}
			// 判据③：active 但对象不在 ⇒ 分享 404。
			if !j.svc.FileExists(ctx, b.ObjectKey) {
				counts[DriftActiveObjectMissing]++
			}
		case model.BlobPurged:
			// 判据④：终态说已删、对象仍在 ⇒ 泄漏。
			if j.svc.FileExists(ctx, b.ObjectKey) {
				counts[DriftPurgedObjectPresent]++
			}
		}
		// trashed/purge 走回收流水线自愈，pending/uploading/committed 尚无
		// 分享引用（或对象在途），均不属四类漂移，跳过。
	}

	// 判据②：路径有引用、账本无行（按去重路径计数）。
	for p := range referenced {
		if !blobKeys[p] {
			counts[DriftMissingLedger]++
		}
	}

	// 观测：成功才整表覆盖刷新（失败不刷——半份数据覆盖成假 0 比陈旧读数更害人）。
	metrics.SetBlobReconcileDrift(counts)
	if total := counts[DriftDanglingLedger] + counts[DriftMissingLedger] +
		counts[DriftActiveObjectMissing] + counts[DriftPurgedObjectPresent]; total > 0 {
		logger.Info("blob drift detected", zap.Int("total", total), zap.Any("drift", counts))
	}
	return counts, nil
}
