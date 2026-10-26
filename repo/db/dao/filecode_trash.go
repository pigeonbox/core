// filecode_trash.go 回收站/软删生命周期簇（事务软删/恢复/硬删与对账查询）。
package dao

import (
	"context"

	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm"
)

// DeleteByIDTx 事务内软删单条分享主记录（原子性由事务保证；
// app 层经此删除，不再直接持有 gorm 会话）。
func (r *FileCodeRepository) DeleteByIDTx(ctx context.Context, id uint) error {
	return r.db().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return tx.Delete(&model.FileCode{}, id).Error
	})
}

// ListAllIncludingDeleted 全量拉取分享记录（含软删）——物理文件对账的引用集：
// 可恢复（软删）分享的物理文件不算孤儿。
func (r *FileCodeRepository) ListAllIncludingDeleted(ctx context.Context) ([]*model.FileCode, error) {
	var rows []*model.FileCode
	if err := r.db().WithContext(ctx).Unscoped().
		Model(&model.FileCode{}).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// CountAlive 存活分享数（deleted_at IS NULL）。
func (r *FileCodeRepository) CountAlive(ctx context.Context) (int64, error) {
	var n int64
	err := r.db().WithContext(ctx).Model(&model.FileCode{}).Count(&n).Error
	return n, err
}

// CountSoftDeleted 软删分享数（deleted_at 非空）——回收站/审计用。
func (r *FileCodeRepository) CountSoftDeleted(ctx context.Context) (int64, error) {
	var n int64
	err := r.db().WithContext(ctx).Unscoped().Model(&model.FileCode{}).
		Where("deleted_at IS NOT NULL").Count(&n).Error
	return n, err
}

// RestoreByIDs 从回收站恢复（软删 → 存活）：deleted_at 置空。
// 仅作用于当前处于软删态的行；返回受影响行数（主表口径）。
//
// ⚠️ 2026-10-26 审计修复（P1）：子表 file_code_files 是**文件清单的唯一真相源**
// （v0.32.14 起虚拟行退役），删除侧（admin.BatchDeleteFiles）会**软删子行**；
// 此前恢复只 UPDATE 主表 ⇒ 子行仍软删 ⇒ 恢复后取件页清单为空、zip 无内容、
// 逐文件下载 404（「恢复」只恢复了一半）。
// ⇒ 同一事务内一并恢复该批分享的软删子行。
//
// 只动 `deleted_at IS NOT NULL` 的行：硬删行没有软删标记，不会被误复活。
func (r *FileCodeRepository) RestoreByIDs(ctx context.Context, ids []uint) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var restored int64
	err := r.db().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Unscoped().Model(&model.FileCode{}).
			Where("id IN ? AND deleted_at IS NOT NULL", ids).
			Update("deleted_at", nil)
		if res.Error != nil {
			return res.Error
		}
		restored = res.RowsAffected
		// 子行同批恢复（子表 = 文件清单唯一真相源 ⇒ 与主表同生共死）
		if err := tx.Unscoped().Model(&model.FileCodeFile{}).
			Where("file_code_id IN ? AND deleted_at IS NOT NULL", ids).
			Update("deleted_at", nil).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return restored, nil
}

// GetByIDUnscoped 按 id 查（含软删行）——回收站/彻底删除前置校验用。
func (r *FileCodeRepository) GetByIDUnscoped(ctx context.Context, id uint) (*model.FileCode, error) {
	var fc model.FileCode
	err := r.db().WithContext(ctx).Unscoped().Where("id = ?", id).First(&fc).Error
	if err != nil {
		return nil, err
	}
	return &fc, nil
}

// HardDeleteByIDs 物理删除 DB 行（回收站「彻底删除」用；对象删除由调用方负责）。
// 子文件行一并硬删（彻底删除不留软删残留）。
func (r *FileCodeRepository) HardDeleteByIDs(ctx context.Context, ids []uint) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	res := r.db().WithContext(ctx).Unscoped().
		Where("id IN ?", ids).Delete(&model.FileCode{})
	_ = r.db().WithContext(ctx).Unscoped().
		Where("file_code_id IN ?", ids).Delete(&model.FileCodeFile{}).Error
	return res.RowsAffected, res.Error
}
