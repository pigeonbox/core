// filecode_usershares.go 用户面「我的分享」簇（列表筛选/批量软删/批量延期/恢复）。
package dao

import (
	"context"
	"time"

	"github.com/pigeonbox/core/repo/db/model"
)

func (r *FileCodeRepository) CountByUserID(ctx context.Context, userID uint) (int64, error) {
	var count int64
	err := r.db().WithContext(ctx).Model(&model.FileCode{}).Where("user_id = ?", userID).Count(&count).Error
	return count, err
}

func (r *FileCodeRepository) GetTotalSizeByUserID(ctx context.Context, userID uint) (int64, error) {
	var totalSize int64
	err := r.db().WithContext(ctx).Model(&model.FileCode{}).
		Where("user_id = ?", userID).
		Select("COALESCE(SUM(size), 0)").
		Scan(&totalSize).Error
	return totalSize, err
}

func (r *FileCodeRepository) GetByUserID(ctx context.Context, userID uint, fileID uint) (*model.FileCode, error) {
	var fileCode model.FileCode
	err := r.db().WithContext(ctx).Where("id = ? AND user_id = ?", fileID, userID).First(&fileCode).Error
	if err != nil {
		return nil, err
	}
	return &fileCode, nil
}

func (r *FileCodeRepository) GetFilesByUserID(ctx context.Context, userID uint) ([]*model.FileCode, error) {
	var files []*model.FileCode
	err := r.db().WithContext(ctx).Where("user_id = ?", userID).Order("created_at DESC").Find(&files).Error
	return files, err
}

func (r *FileCodeRepository) GetFilesByUserIDWithPagination(ctx context.Context, userID uint, page, pageSize int) ([]*model.FileCode, int64, error) {
	page, pageSize = normalizePage(page, pageSize)

	// 构建查询条件
	query := r.db().WithContext(ctx).Model(&model.FileCode{}).Where("user_id = ?", userID)

	return paginate[model.FileCode](query.Order("created_at DESC"), page, pageSize)
}

func (r *FileCodeRepository) DeleteByUserID(ctx context.Context, userID uint) error {
	return r.db().WithContext(ctx).Where("user_id = ?", userID).Delete(&model.FileCode{}).Error
}

// UserShareFilter 用户分享列表筛选条件
type UserShareFilter struct {
	Status   string // all / active / expired / text / file / deleted / viewed
	Search   string // 模糊搜索 code / 文件名
	Page     int
	PageSize int
}

// GetUserSharesWithFilter 获取用户的分享列表（带筛选）
//   - "active": 未过期且有剩余次数
//   - "expired": 时间过期 或 次数用尽
//   - "text": 文本分享（无文件路径且 Text 非空；文件分享的 Text 存原始文件名，不能只看 Text）
//   - "file": 文件分享（file_path 非空）
//   - "deleted": 软删除的（deleted_at != null）
//   - "viewed": 至少被取件过一次（viewer_at 非空；取件历史页用，total 必须与
//     列表同源——前端曾拉全量再客户端过滤，空表却显示全量分页总数）
//   - "all" / "": 不过滤状态
func (r *FileCodeRepository) GetUserSharesWithFilter(ctx context.Context, userID uint, filter UserShareFilter) ([]*model.FileCode, int64, error) {
	page, pageSize := normalizeUserSharePage(filter.Page, filter.PageSize)

	now := time.Now()
	q := r.db().WithContext(ctx).Model(&model.FileCode{}).Where("user_id = ?", userID)

	switch filter.Status {
	case "deleted":
		// 只看已删除
		q = q.Unscoped().Where("deleted_at IS NOT NULL")
	case "active":
		q = q.Where("(expired_at IS NULL OR expired_at > ?) AND (expired_count <> 0)", now)
	case "expired":
		q = q.Where("((expired_at IS NOT NULL AND expired_at <= ?) OR expired_count = 0)", now)
	case "text":
		q = q.Where("(file_path IS NULL OR file_path = '') AND text IS NOT NULL AND text != ''")
	case "file":
		q = q.Where("file_path IS NOT NULL AND file_path != ''")
	case "viewed":
		// 取件历史：至少被取件过一次
		q = q.Where("viewer_at IS NOT NULL")
	}

	if filter.Search != "" {
		like, esc := LikeContains(filter.Search)
		// 文件分享的原始文件名存 text（uuid_file_name 常为空），搜索需覆盖 text；
		// pickup_code 落库后列表主码即 6 位取件码，按码搜索必须覆盖（2026-10-08）
		q = q.Where("code LIKE ? OR uuid_file_name LIKE ? OR text LIKE ? OR pickup_code LIKE ? OR file_path LIKE ? "+esc,
			like, like, like, like, like)
	}

	return paginate[model.FileCode](q.Order("created_at DESC"), page, pageSize)
}

// BatchSoftDeleteByCodes 按 code 列表软删除（限定 userID 防止越权）
// 返回 (受影响行数, error)
func (r *FileCodeRepository) BatchSoftDeleteByCodes(ctx context.Context, userID uint, codes []string) (int, error) {
	if len(codes) == 0 {
		return 0, nil
	}
	res := r.db().WithContext(ctx).Model(&model.FileCode{}).
		Where("user_id = ? AND code IN ?", userID, codes).
		Update("deleted_at", time.Now())
	if res.Error != nil {
		return 0, res.Error
	}
	return int(res.RowsAffected), nil
}

// BatchExtendByCodes 批量延期
// newExpireAt 为 nil 表示永久（清空 expired_at 字段）
func (r *FileCodeRepository) BatchExtendByCodes(ctx context.Context, userID uint, codes []string, newExpireAt *time.Time) (int, error) {
	if len(codes) == 0 {
		return 0, nil
	}
	updates := map[string]interface{}{}
	if newExpireAt == nil {
		updates["expired_at"] = nil
		updates["expired_count"] = -1
	} else {
		updates["expired_at"] = *newExpireAt
		// 次数设为 -1（无限）让延期后能继续取
		updates["expired_count"] = -1
	}
	res := r.db().WithContext(ctx).Model(&model.FileCode{}).
		Where("user_id = ? AND code IN ?", userID, codes).
		Updates(updates)
	if res.Error != nil {
		return 0, res.Error
	}
	return int(res.RowsAffected), nil
}

// RestoreByCode 恢复软删除的分享（仅 owner）。返回实际恢复行数：
// 0 = 该分享不在回收站（未软删或不存在）。调用方必须对 0 显式报错——
// 此前 0 行恢复也返回 nil，硬删后 restore 表现为 200 静默 no-op，
// 集成误判已恢复（2026-10-08 修复；与 v0.11.1 hard-delete 同款收口）。
func (r *FileCodeRepository) RestoreByCode(ctx context.Context, userID uint, code string) (int64, error) {
	res := r.db().WithContext(ctx).Unscoped().Model(&model.FileCode{}).
		Where("user_id = ? AND code = ? AND deleted_at IS NOT NULL", userID, code).
		Update("deleted_at", nil)
	if res.Error != nil {
		return 0, res.Error
	}
	return res.RowsAffected, nil
}
