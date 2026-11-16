// filecode_reclaim_guard.go 回收路径守卫的收紧口径（core#172 守卫②降级）。
//
// # 为什么可以收紧
//
// 回填全量验证（2026-11-16，10.10.10.3 生产库副本2.9MB/253 分享）：
// RunDataMigrations真实启动路径 scanned=253 missing=553 created=553 failed=0，
// 覆盖归零、二次运行零差集、配额零漂移、软删行全覆盖 ⇒ 「active 行 ⇒ 有账本
// ref」在存量上成立（启动回填 + 写路径双写承接），路径守卫对 active+有 ref 行
// 的重复扫描（旧 CountFilePathReferences 全量口径）可以交还给守卫①。
//
// # 本方法数什么（两类，缺一不删的方向不变）
//
//	① 软删行（主表+子表，deleted_at 非空，任何覆盖态）——字面口径「仅软删行」：
//	   回收站仍占对象；软删不释放的设计下 ref 应为 active，但异常/历史数据
//	   可能 released/缺行，路径守卫是残余兜底；
//	② 无账本行的任何状态行（按 (file_code_id, object_key) 对齐 NOT EXISTS）——
//	   回填是 best-effort（Failed>0 真实可能：单对失败继续），丢掉这层会把
//	   「回填失败」直接变成「误删事故」；验证 failed=0 ⇒ 正常态该类恒为 0，
//	   不违背收紧意图（宁泄漏勿误删优先于口径纯度）。
//
// 查不清不删、空路径返回 0（无对象可挡）与旧口径一致。
// 消费方：app/admin/blob_gc.go ReclaimBlobs 守卫②。#196 的 gcjob 面仍用
// CountFilePathReferences（另一语义面，不随本片降级）。
package dao

import (
	"context"

	"github.com/pigeonbox/core/repo/db/model"
)

// CountReclaimPathGuard 回收路径守卫计数：软删行 或 无账本行（见文件头）。
// 与 CountFilePathReferences 的差异 = 不再数「active 且有账本 ref」的行
// （其保护由守卫① CountActiveRefs 承接）。
func (r *FileCodeRepository) CountReclaimPathGuard(ctx context.Context, filePath string) (int64, error) {
	if filePath == "" {
		return 0, nil
	}
	// 该路径的账本覆盖集合（任何 state 的 ref；released 也算已覆盖——与
	// BackfillLegacyRefs 差集口径同源，避免对异常数据重试复活而报错）。
	var coveredIDs []uint
	if err := r.db().WithContext(ctx).
		Table("file_references").
		Joins("JOIN blobs ON blobs.id = file_references.blob_id").
		Where("blobs.object_key = ?", filePath).
		Pluck("file_references.file_code_id", &coveredIDs).Error; err != nil {
		return 0, err
	}
	covered := make(map[uint]bool, len(coveredIDs))
	for _, id := range coveredIDs {
		covered[id] = true
	}

	var rows []model.FileCode
	if err := r.db().WithContext(ctx).Unscoped().
		Where("file_path = ?", filePath).
		Find(&rows).Error; err != nil {
		return 0, err
	}
	var n int64
	for i := range rows {
		if rows[i].GetFilePath() != filePath {
			continue // 与 CountFilePathReferences 同款归一化复核
		}
		if rows[i].DeletedAt.Valid || !covered[rows[i].ID] {
			n++
		}
	}

	var children []model.FileCodeFile
	if err := r.db().WithContext(ctx).Unscoped().
		Where("file_path = ?", filePath).
		Find(&children).Error; err != nil {
		return 0, err
	}
	for i := range children {
		if children[i].FilePath != filePath {
			continue
		}
		if children[i].DeletedAt.Valid || !covered[children[i].FileCodeID] {
			n++
		}
	}
	return n, nil
}
