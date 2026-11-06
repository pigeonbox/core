// filecode_trash.go 回收站/软删生命周期簇（事务软删/恢复/硬删与对账查询）。
package dao

import (
	"context"

	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm"
)

// DeleteByIDTx 事务内软删单条分享（主记录 + 子行，原子性由事务保证；
// app 层经此删除，不再直接持有 gorm 会话）。
func (r *FileCodeRepository) DeleteByIDTx(ctx context.Context, id uint) error {
	return r.db().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := softDeleteChildRows(tx, []uint{id}); err != nil {
			return err
		}
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
// （v0.32.14 起虚拟行退役），删除侧会**软删子行**（2026-11-06 起全部软删路径
// 在 dao 层同事务级联，见 softDeleteChildRows）；
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
//
// 2026-11-04（core#62）：改为**同一事务 + 子先父后 + 错误不再被吞**。
// 此前：① 父先删、子后删（顺序与「先查父 id→删子→删父」相反）；② 子表删除的
// 错误被空赋值吞掉 ⇒ 子表删失败也照样返回成功 ⇒ 静默留孤儿行。
func (r *FileCodeRepository) HardDeleteByIDs(ctx context.Context, ids []uint) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var deleted int64
	err := r.db().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := hardDeleteChildRows(tx, ids); err != nil {
			return err
		}
		res := tx.Unscoped().Where("id IN ?", ids).Delete(&model.FileCode{})
		if res.Error != nil {
			return res.Error
		}
		deleted = res.RowsAffected
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// softDeleteChildRows 事务内软删 file_code_files 子行（主表 id 集合）。
//
// 「同生共死」的软删侧：凡软删父行必同事务软删子行（此前只有 app 层散落 4 处
// 在物理清理分支里补刀，且以 storage!=nil 为前提 ⇒ 用户面回收站入口
// BatchSoftDeleteByCodes 与单条 DeleteByIDTx 完全不级联 ⇒ 同一个「软删」状态
// 出现两种子行状态）。已软删的子行再删一次是 no-op（幂等）。
func softDeleteChildRows(tx *gorm.DB, ids []uint) error {
	if len(ids) == 0 {
		return nil
	}
	return tx.Where("file_code_id IN ?", ids).Delete(&model.FileCodeFile{}).Error
}

// hardDeleteChildRows 事务内物理删除 file_code_files 子行（主表 id 集合）。
//
// 子表与主表「同生共死」：主表物理删除时子表必须一起物理删除，否则
// ① 子行成孤儿（父行不存在）；② janitor 对账引用集仍含子行路径
// ⇒ 被删分享的物理文件永不回收（core#62）。
//
// 只删「物理行」（Unscoped）——软删子行同样要清：彻底删除不留软删残留。
func hardDeleteChildRows(tx *gorm.DB, ids []uint) error {
	if len(ids) == 0 {
		return nil
	}
	return tx.Unscoped().
		Where("file_code_id IN ?", ids).
		Delete(&model.FileCodeFile{}).Error
}
