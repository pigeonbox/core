// filecode_pickup.go 码唯一性检查与 6 位取件码簇（铸造/占用检查/反查/作废）。
package dao

import (
	"context"

	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm"
)

// CodeOccupancy 分享码占用状态（预检口径，见 CheckCodeOccupancy）。
type CodeOccupancy int

const (
	// CodeFree 空闲，可占用。
	CodeFree CodeOccupancy = iota
	// CodeOccupiedAlive 被存活行占用。
	CodeOccupiedAlive
	// CodeOccupiedRecycleBin 仅被回收站（软删）行占用——purge 前不可复用。
	CodeOccupiedRecycleBin
)

// CheckCodeOccupancy 分享码占用检查（**与 file_codes.code 唯一索引同域**）。
//
// core#112（2026-11-11）：唯一索引不区分软删 ⇒ 软删行在 purge 前仍占码；
// 此前预检（First 不带 Unscoped）只看存活行 ⇒ 预检放行、Create 撞索引 ⇒
// 报「已被占用」但回收站里找不到占用者（幽灵占用）。预检与索引同域后语义
// 自洽：软删行占码期间拒绝复用（文案点名回收站），purge 后释放。
//
// 软删行本身零改动：恢复 / purge / 引用计数语义均不受影响（预检只是把
// 「索引已有的事实」提前说出口）。跨库口径：判据是纯 SQL 条件（Unscoped
// = 去掉自动的 deleted_at IS NULL），sqlite/mysql/pg 三家同语义，无迁移需求。
func (r *FileCodeRepository) CheckCodeOccupancy(ctx context.Context, code string, excludeID uint) (CodeOccupancy, error) {
	return r.checkCodeOccupancy(ctx, "code = ? AND id != ?", code, excludeID)
}

// CheckCodeOccupancyFolded = CheckCodeOccupancy 的大小写折叠形态（UPPER 归一，
// 查询折叠开启时自定义口令创建用它兜底查重，防 ABC/abc 并存后折叠查询互撞）。
// 折叠判定同样含软删行：软删行恢复后即回到折叠解析域，宽松预检会在恢复后撞车。
func (r *FileCodeRepository) CheckCodeOccupancyFolded(ctx context.Context, code string, excludeID uint) (CodeOccupancy, error) {
	return r.checkCodeOccupancy(ctx, "UPPER(code) = UPPER(?) AND id != ?", code, excludeID)
}

func (r *FileCodeRepository) checkCodeOccupancy(ctx context.Context, matchExpr, code string, excludeID uint) (CodeOccupancy, error) {
	var rows []model.FileCode
	// Unscoped：与唯一索引同域（含软删行）；只取判定所需两列。
	err := r.db().WithContext(ctx).Unscoped().Model(&model.FileCode{}).
		Select("id", "deleted_at").
		Where(matchExpr, code, excludeID).
		Find(&rows).Error
	if err != nil {
		return CodeFree, err
	}
	for _, row := range rows {
		if !row.DeletedAt.Valid {
			return CodeOccupiedAlive, nil
		}
	}
	if len(rows) > 0 {
		return CodeOccupiedRecycleBin, nil
	}
	return CodeFree, nil
}

// CheckCodeExists 占用检查（bool 便捷口径 = CheckCodeOccupancy != CodeFree）。
// **含软删行**（与唯一索引同域）——语义与理由见 CheckCodeOccupancy。
func (r *FileCodeRepository) CheckCodeExists(ctx context.Context, code string, excludeID uint) (bool, error) {
	occ, err := r.CheckCodeOccupancy(ctx, code, excludeID)
	return occ != CodeFree, err
}

// CheckCodeExistsFolded 大小写折叠占用检查（bool 便捷口径，含软删行）。
func (r *FileCodeRepository) CheckCodeExistsFolded(ctx context.Context, code string, excludeID uint) (bool, error) {
	occ, err := r.CheckCodeOccupancyFolded(ctx, code, excludeID)
	return occ != CodeFree, err
}

// SetPickupCode 铸造 6 位取件码落库（file_codes.pickup_code 列，真相源）。
// 先查占用再更新：唯一索引兜底跨分享撞码，占用返回 ok=false 由调用方换码重试。
//
// core#112 同族修正：pickup_code 同为 uniqueIndex（不区分软删）⇒ 软删行在
// purge 前**仍占码**（旧注释「软删行天然不参与、回收站分享的码可被新分享重新
// 生成」与唯一索引语义相悖，从未成立——预检放行后 UpdateColumn 一样撞索引）。
// 预检改为与索引同域（CheckPickupCodeExists 含软删行），撞码统一走换码重试。
func (r *FileCodeRepository) SetPickupCode(ctx context.Context, code, pickupCode string) (bool, error) {
	taken, err := r.CheckPickupCodeExists(ctx, pickupCode)
	if err != nil || taken {
		return false, err
	}
	res := r.db().WithContext(ctx).Model(&model.FileCode{}).
		Where("code = ?", code).
		UpdateColumn("pickup_code", pickupCode)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// CheckPickupCodeExists 取件码占用检查。
//
// core#112 同族：与 pickup_code 唯一索引同域（Unscoped ⇒ 含软删行，purge 后释放）。
// 此前只看存活行 ⇒ 预检说空闲、落库撞索引（幽灵占用），理由同 CheckCodeOccupancy。
func (r *FileCodeRepository) CheckPickupCodeExists(ctx context.Context, pickupCode string) (bool, error) {
	var existing model.FileCode
	err := r.db().WithContext(ctx).Unscoped().Where("pickup_code = ?", pickupCode).First(&existing).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// GetByPickupCode 按取件码反查分享（KV 缓存未命中时的 DB 回退路径：
// 内存模式重启丢 KV、永久分享不写 KV，均由此解析）。
func (r *FileCodeRepository) GetByPickupCode(ctx context.Context, pickupCode string) (*model.FileCode, error) {
	var fileCode model.FileCode
	err := r.db().WithContext(ctx).Where("pickup_code = ?", pickupCode).First(&fileCode).Error
	if err != nil {
		return nil, err
	}
	return &fileCode, nil
}

// ClearPickupCode 作废取件码落库侧（Cancel 消费）：KV 映射与 DB 列同步清除。
func (r *FileCodeRepository) ClearPickupCode(ctx context.Context, pickupCode string) error {
	return r.db().WithContext(ctx).Model(&model.FileCode{}).
		Where("pickup_code = ?", pickupCode).
		UpdateColumn("pickup_code", nil).Error
}
