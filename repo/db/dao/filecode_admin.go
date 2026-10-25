// filecode_admin.go 管理端治理/仪表盘簇（批量操作/富统计/组合过滤/健康口径/状态机）。
package dao

import (
	"context"
	"fmt"
	"time"

	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm"
)

// ==================== 管理端增强（批量操作 / 富统计） ====================

// BatchDeleteByIDs 批量删除（软删），返回受影响行数
func (r *FileCodeRepository) BatchDeleteByIDs(ctx context.Context, ids []uint) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	res := r.db().WithContext(ctx).Where("id IN ?", ids).Delete(&model.FileCode{})
	return int(res.RowsAffected), res.Error
}

// UpdateExpireByID 管理员改单条过期时间/剩余次数
func (r *FileCodeRepository) UpdateExpireByID(ctx context.Context, id uint, expireAt *time.Time, expiredCount *int) error {
	updates := map[string]interface{}{}
	if expireAt != nil {
		updates["expired_at"] = *expireAt
	}
	if expiredCount != nil {
		updates["expired_count"] = *expiredCount
	}
	if len(updates) == 0 {
		return nil
	}
	return r.db().WithContext(ctx).Model(&model.FileCode{}).Where("id = ?", id).Updates(updates).Error
}

// BatchExtendByIDsAdmin 管理员批量延期（不限 owner）
func (r *FileCodeRepository) BatchExtendByIDsAdmin(ctx context.Context, ids []uint, newExpireAt time.Time) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	res := r.db().WithContext(ctx).Model(&model.FileCode{}).
		Where("id IN ?", ids).
		Updates(map[string]interface{}{"expired_at": newExpireAt, "expired_count": -1})
	return int(res.RowsAffected), res.Error
}

// SuffixStat 文件后缀统计项
type SuffixStat struct {
	Suffix string `json:"suffix"`
	Count  int64  `json:"count"`
}

// TopSuffixes 上传文件后缀 TOP N（Dashboard 文件类型分布）
func (r *FileCodeRepository) TopSuffixes(ctx context.Context, limit int) ([]*SuffixStat, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	var rows []*SuffixStat
	err := r.db().WithContext(ctx).Model(&model.FileCode{}).
		Select("suffix, COUNT(*) AS count").
		Where("suffix <> ''").
		Group("suffix").
		Order("count DESC").
		Limit(limit).
		Scan(&rows).Error
	return rows, err
}

// CountCreatedBetween 统计 [from, to) 创建数（昨日对比等）
func (r *FileCodeRepository) CountCreatedBetween(ctx context.Context, from, to time.Time) (int64, error) {
	var count int64
	err := r.db().WithContext(ctx).Model(&model.FileCode{}).
		Where("created_at >= ? AND created_at < ?", from, to).
		Count(&count).Error
	return count, err
}

// TrendByDay 按天统计上传数（Dashboard 趋势序列，from 起含当天）
func (r *FileCodeRepository) TrendByDay(ctx context.Context, from time.Time) ([]DayCount, error) {
	var rows []DayCount
	err := r.db().WithContext(ctx).Model(&model.FileCode{}).
		Select("DATE(created_at) AS date, COUNT(*) AS count").
		Where("created_at >= ?", from).
		Group("DATE(created_at)").Order("DATE(created_at)").
		Scan(&rows).Error
	return rows, err
}

// SumUsedCount 累计取件/下载次数
func (r *FileCodeRepository) SumUsedCount(ctx context.Context) (int64, error) {
	var total int64
	err := r.db().WithContext(ctx).Model(&model.FileCode{}).
		Select("COALESCE(SUM(used_count), 0)").Scan(&total).Error
	return total, err
}

// CountByUploadType 按上传类型统计（anonymous/authenticated/presign_*）
func (r *FileCodeRepository) CountByUploadType(ctx context.Context, uploadType string) (int64, error) {
	var count int64
	err := r.db().WithContext(ctx).Model(&model.FileCode{}).
		Where("upload_type = ?", uploadType).Count(&count).Error
	return count, err
}

// CountExpired 统计已过期文件数
func (r *FileCodeRepository) CountExpired(ctx context.Context) (int64, error) {
	var count int64
	err := r.db().WithContext(ctx).Model(&model.FileCode{}).
		Where("(expired_at IS NOT NULL AND expired_at < ?) OR expired_count = 0", time.Now()).
		Count(&count).Error
	return count, err
}

// ==================== 管理端治理（2026-10-03）：组合过滤 + 状态机 ====================

// ListWithFilter 管理端文件列表：组合过滤 + 分页（此前仅 keyword 模糊匹配，
// 无法按上传者/IP/类型/大小/时间/状态定位滥用资源）。
func (r *FileCodeRepository) ListWithFilter(ctx context.Context, q model.FileCodeQuery) ([]*model.FileCode, int64, error) {
	page, pageSize := clampPage(q.Page, q.PageSize, MaxPageSize)

	query := r.db().WithContext(ctx).Model(&model.FileCode{})

	// 回收站（软删）筛选：only=仅已删 / all=含已删；默认仅存活（gorm 默认 scope）。
	// Unscoped 同时解除软删过滤，后续条件在可见全集上叠加。
	switch q.Deleted {
	case "only":
		query = query.Unscoped().Where("file_codes.deleted_at IS NOT NULL")
	case "all":
		query = query.Unscoped()
	}

	if q.Keyword != "" {
		like, esc := LikeContains(q.Keyword)
		// pickup_code 同码搜索（管理端按对方口述的 6 位码定位分享）
		// prefix/suffix 死列已退役（2026-10-26，库中恒空且无写入方）
		query = query.Where("code LIKE ? OR uuid_file_name LIKE ? OR text LIKE ? OR pickup_code LIKE ? OR file_path LIKE ? "+esc,
			like, like, like, like, like)
	}
	if q.UserID != nil {
		query = query.Where("user_id = ?", *q.UserID)
	}
	if q.UploadType != "" {
		query = query.Where("upload_type = ?", q.UploadType)
	}
	if q.OwnerIP != "" {
		query = query.Where("owner_ip = ?", q.OwnerIP)
	}
	if q.Status != "" {
		query = query.Where("status = ?", q.Status)
	}
	if q.MinSize != nil {
		query = query.Where("size >= ?", *q.MinSize)
	}
	if q.MaxSize != nil {
		query = query.Where("size <= ?", *q.MaxSize)
	}
	if q.CreatedAfter != nil {
		query = query.Where("created_at >= ?", *q.CreatedAfter)
	}
	if q.CreatedBefore != nil {
		query = query.Where("created_at <= ?", *q.CreatedBefore)
	}
	if q.Expired != nil {
		now := time.Now()
		if *q.Expired {
			// 注意外层括号：缺了会让 OR 逃逸出 AND 链，绕过 status/ip 等全部过滤
			query = query.Where("((expired_at IS NOT NULL AND expired_at < ?) OR expired_count = 0)", now)
		} else {
			query = query.Where("((expired_at IS NULL OR expired_at >= ?) AND expired_count <> 0)", now)
		}
	}
	if q.Health != "" {
		query = applyHealthFilter(query, q.Health)
	}

	return paginate[model.FileCode](query.Order("created_at DESC"), page, pageSize)
}

// applyHealthFilter 文件健康洞察过滤（与 CountByHealth 同一口径，勿单独改动）：
//   - active        可取件（未过期，含永久）
//   - expired       已过期（时间或次数耗尽）
//   - expiring_soon 24h 内即将过期（未过期且 expired_at 落在未来 24h）
//   - never_picked  创建后从未被取件且未过期
//   - forever       永久有效（无过期时间且不限次数）
func applyHealthFilter(query *gorm.DB, health string) *gorm.DB {
	now := time.Now()
	switch health {
	case "active":
		return query.Where("((expired_at IS NULL OR expired_at >= ?) AND expired_count <> 0)", now)
	case "expired":
		return query.Where("((expired_at IS NOT NULL AND expired_at < ?) OR expired_count = 0)", now)
	case "expiring_soon":
		return query.Where("expired_at IS NOT NULL AND expired_at >= ? AND expired_at < ? AND expired_count <> 0", now, now.Add(24*time.Hour))
	case "never_picked":
		return query.Where("used_count = 0 AND ((expired_at IS NULL OR expired_at >= ?) AND expired_count <> 0)", now)
	case "forever":
		return query.Where("expired_at IS NULL AND expired_count < 0")
	default:
		return query
	}
}

// CountByHealth 按健康维度计数（admin 仪表盘洞察卡；单次扫描全维度，口径见 applyHealthFilter）。
func (r *FileCodeRepository) CountByHealth(ctx context.Context) (active, expired, expiringSoon, neverPicked, forever int64, err error) {
	now := time.Now()
	base := func() *gorm.DB {
		return r.db().WithContext(ctx).Model(&model.FileCode{}).
			Where("((expired_at IS NULL OR expired_at >= ?) AND expired_count <> 0)", now)
	}
	if err = base().Count(&active).Error; err != nil {
		return
	}
	expiredQ := r.db().WithContext(ctx).Model(&model.FileCode{}).
		Where("((expired_at IS NOT NULL AND expired_at < ?) OR expired_count = 0)", now)
	if err = expiredQ.Count(&expired).Error; err != nil {
		return
	}
	if err = base().Where("expired_at IS NOT NULL AND expired_at < ?", now.Add(24*time.Hour)).
		Count(&expiringSoon).Error; err != nil {
		return
	}
	if err = base().Where("used_count = 0").Count(&neverPicked).Error; err != nil {
		return
	}
	err = r.db().WithContext(ctx).Model(&model.FileCode{}).
		Where("expired_at IS NULL AND expired_count < 0").Count(&forever).Error
	return
}

// UpdateStatusByIDs 批量更新管控状态（单个/批量禁用、恢复共用）。
// status 取值经 model.ValidShareStatus 白名单校验；返回受影响行数。
func (r *FileCodeRepository) UpdateStatusByIDs(ctx context.Context, ids []uint, status string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if !model.ValidShareStatus(status) {
		return 0, fmt.Errorf("非法的分享状态: %s", status)
	}
	res := r.db().WithContext(ctx).Model(&model.FileCode{}).
		Where("id IN ?", ids).
		Update("status", status)
	return res.RowsAffected, res.Error
}
