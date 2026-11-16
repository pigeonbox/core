// blob_backfill.go 存量数据回填（core#172 第二片）：给「只有 file_codes.file_path、
// 无 Blob/Reference 行」的老数据补账本行。
//
// # 口径与取舍
//
//   - **幂等可重入**：差集驱动（只补缺失的 (分享, 路径) 对），EnsureBlobWithRef
//     自带唯一索引幂等 ⇒ 中断后再跑不产生重复行、不重复计数；已覆盖的对
//     （含 released ref——数据异常时避免复活报错）一律跳过。
//   - **扫描范围 = file_codes 现存行（Unscoped，含软删）**：回收站行仍占对象，
//     硬删时要释放 ⇒ 必须有 active ref；文本分享（file_path 空）无对象，跳过。
//   - **读路径兼容读**：回填是补账，不改任何读语义——存量分享在回填前靠
//     ReclaimBlobs 守卫②（路径口径 CountFilePathReferences，含无账本行的
//     存量分享）保护不被误删；回填后账本口径与路径口径对齐。本片不降级
//     守卫②（拆分清单第 1 项的降级留给回填全量验证后的后续 PR）。
//   - **配额零漂移**：只写 blobs/file_references 两张账本表，不碰 file_codes
//     的 size 聚合 ⇒ GetTotalSize/GetTotalSizeIncludingDeleted 口径不变
//     （负对照测试钉死）。
//   - **best-effort**：单对失败仅计数不中断（泄漏方向：缺账本行由守卫②
//     与 janitor 路径口径兜底）。
package dao

import (
	"context"

	"github.com/pigeonbox/core/repo/db/model"
)

// BackfillStats 回填统计（可观测 + 测试断言）。
type BackfillStats struct {
	// Scanned 扫描的分享行数（含软删）。
	Scanned int
	// Missing 差集大小（需要补的 (分享, 路径) 对）。
	Missing int
	// Created 实际补齐的引用数。
	Created int
	// Failed 补齐失败数（best-effort，单对失败不中断）。
	Failed int
}

// BackfillLegacyRefs 存量账本回填（幂等、可重入、可后台跑）。
//
// 差集 = 「file_codes 现存行的主表/子表路径」−「已有的 file_reference
// （任意 state，按 file_code_id + blobs.object_key 对齐）」，逐对
// EnsureBlobWithRef 补齐。重复调用：第二次差集为空，Created=0。
func (r *BlobRepository) BackfillLegacyRefs(ctx context.Context) (BackfillStats, error) {
	var st BackfillStats

	// 1. 存量路径：主表 + 子表（Unscoped：软删行仍占对象，硬删要释放）。
	type pathRow struct {
		FileCodeID uint
		FilePath   string
		Size       int64
		FileHash   string
	}
	var paths []pathRow
	if err := r.db().WithContext(ctx).Unscoped().Model(&model.FileCode{}).
		Where("file_path <> ''").
		Select("id AS file_code_id, file_path, size, file_hash").
		Scan(&paths).Error; err != nil {
		return st, err
	}
	var children []pathRow
	if err := r.db().WithContext(ctx).Unscoped().Model(&model.FileCodeFile{}).
		Where("file_path <> ''").
		Select("file_code_id, file_path, size, file_hash").
		Scan(&children).Error; err != nil {
		return st, err
	}
	paths = append(paths, children...)

	// 2. 已覆盖集合：任何 state 的 ref（released 也算已覆盖——避免对
	//    「引用已 released」的异常数据重试复活而报错）。
	type refRow struct {
		FileCodeID uint
		ObjectKey  string
	}
	var covered []refRow
	if err := r.db().WithContext(ctx).
		Table("file_references").
		Select("file_references.file_code_id, blobs.object_key").
		Joins("JOIN blobs ON blobs.id = file_references.blob_id").
		Scan(&covered).Error; err != nil {
		return st, err
	}
	type key struct {
		id   uint
		path string
	}
	seen := make(map[key]bool, len(covered))
	for _, c := range covered {
		seen[key{c.FileCodeID, c.ObjectKey}] = true
	}

	// 3. 差集逐对补齐（EnsureBlobWithRef 幂等；单对失败 best-effort 继续）。
	shareIDs := make(map[uint]bool, len(paths))
	for _, p := range paths {
		shareIDs[p.FileCodeID] = true
	}
	st.Scanned = len(shareIDs)
	for _, p := range paths {
		if seen[key{p.FileCodeID, p.FilePath}] {
			continue
		}
		st.Missing++
		if _, err := r.EnsureBlobWithRef(ctx, p.FilePath, p.Size, p.FileHash, p.FileCodeID); err != nil {
			st.Failed++
			continue
		}
		st.Created++
	}
	return st, nil
}
