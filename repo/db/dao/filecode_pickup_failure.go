package dao

import (
	"context"
	"time"

	"github.com/pigeonbox/core/repo/db/model"
)

// ==================== 取件失败事件（frontend-core#93，2026-11-16） ====================
//
// 与 pickup_charges 同挂 FileCodeRepository：两者都以「规范分享码」为归属键，
// 且共享注入点（share Service 已持有 fileCodeRepo ⇒ 记录/读取零装配改动）。
// 语义与生命周期见 model.PickupFailure 注释（7 天留存、不落明文 IP、不扣次）。

// CreatePickupFailure 落一条取件失败事件。
// 调用方约定：**只记录，不扣次**；本方法失败只告警不阻断取件主流程（观测面）。
func (r *FileCodeRepository) CreatePickupFailure(ctx context.Context, f *model.PickupFailure) error {
	return r.db().WithContext(ctx).Create(f).Error
}

// ListPickupFailures 某分享的失败事件（最近 limit 条，时间倒序）。
// owner 侧取件时间线的失败数据源。
func (r *FileCodeRepository) ListPickupFailures(ctx context.Context, code string, limit int) ([]model.PickupFailure, error) {
	if limit <= 0 {
		limit = 50
	}
	var rows []model.PickupFailure
	err := r.db().WithContext(ctx).
		Where("code = ?", code).
		Order("failed_at DESC, id DESC").
		Limit(limit).
		Find(&rows).Error
	return rows, err
}

// CountPickupFailures 某分享的失败事件总数（时间线摘要计数；不受 limit 截断影响）。
func (r *FileCodeRepository) CountPickupFailures(ctx context.Context, code string) (int64, error) {
	var n int64
	err := r.db().WithContext(ctx).Model(&model.PickupFailure{}).
		Where("code = ?", code).
		Count(&n).Error
	return n, err
}

// CountDistinctFailureSources 失败来源数（粗粒度指纹去重）。
//
// 隐私口径：只回**去重后的个数**——空指纹（来源未知）不计，
// 指纹本身不出本方法（展示面拿不到，更拿不到明文 IP）。
func (r *FileCodeRepository) CountDistinctFailureSources(ctx context.Context, code string) (int64, error) {
	var n int64
	err := r.db().WithContext(ctx).Model(&model.PickupFailure{}).
		Where("code = ? AND source_hash <> ?", code, "").
		Distinct("source_hash").
		Count(&n).Error
	return n, err
}

// DeleteExpiredPickupFailures 失败事件留存清理：删除 FailedAt 早于 before 的行。
//
// **硬删**（Unscoped）：与 pickup_charges 同理，软删行会让时间线继续展示
// 「已清理」的失败记录；失败事件是短命观测数据，不需要回收站语义。
func (r *FileCodeRepository) DeleteExpiredPickupFailures(ctx context.Context, before time.Time) (int64, error) {
	res := r.db().WithContext(ctx).Unscoped().Where("failed_at < ?", before).Delete(&model.PickupFailure{})
	return res.RowsAffected, res.Error
}

// DeleteOrphanPickupFailures 清理「分享不存活」的孤儿失败事件。
// 判据与 DeleteOrphanPickupCharges 同 SQL：存活 = deleted_at IS NULL ⇒
// 硬删与进回收站的分享，其失败事件一并清（时间线对回收站分享本就 404，
// 留着既无主人可看又占索引）。
func (r *FileCodeRepository) DeleteOrphanPickupFailures(ctx context.Context) (int64, error) {
	res := r.db().WithContext(ctx).Unscoped().
		Where("code NOT IN (SELECT code FROM file_codes WHERE deleted_at IS NULL)").
		Delete(&model.PickupFailure{})
	return res.RowsAffected, res.Error
}
