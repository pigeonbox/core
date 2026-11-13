// filecode_admin.go 管理端治理/仪表盘簇（批量操作/富统计/组合过滤/健康口径/状态机）。
package dao

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/oekit/gormx/crud"
	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm"
)

// ==================== 管理端增强（批量操作 / 富统计） ====================

// BatchDeleteByIDs 批量删除（软删，连带软删子行「同生共死」），返回受影响行数
func (r *FileCodeRepository) BatchDeleteByIDs(ctx context.Context, ids []uint) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var affected int64
	err := r.db().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := softDeleteChildRows(tx, ids); err != nil {
			return err
		}
		res := tx.Where("id IN ?", ids).Delete(&model.FileCode{})
		if res.Error != nil {
			return res.Error
		}
		affected = res.RowsAffected
		return nil
	})
	if err != nil {
		return 0, err
	}
	return int(affected), nil
}

// UpdateExpireByID 管理员改单条过期时间/剩余次数
func (r *FileCodeRepository) UpdateExpireByID(ctx context.Context, id uint, expireAt *time.Time, expiredCount *int) error {
	updates := map[string]interface{}{}
	if expireAt != nil {
		updates["expired_at"] = *expireAt
		// 管理员改了到期时间 ⇒ 清「已提醒」标记（同 BatchExtendByCodes，5.5.0）。
		updates["expiry_reminded_at"] = nil
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
		Updates(map[string]interface{}{
			"expired_at":         newExpireAt,
			"expired_count":      -1,
			"expiry_reminded_at": nil, // 延期 ⇒ 允许再次提醒（5.5.0）
		})
	return int(res.RowsAffected), res.Error
}

// SuffixStat 文件后缀统计项
type SuffixStat struct {
	Suffix string `json:"suffix"`
	Count  int64  `json:"count"`
}

// TopSuffixes 上传文件后缀 TOP N（Dashboard 文件类型分布）
//
// ⚠️ 2026-10-26 修复：原实现是 `SELECT suffix, COUNT(*) … GROUP BY suffix`
// ——查的是**已退役的 suffix 列** ⇒ ①该列本就恒空（widget 早就是死的）
// ②退役后变成「SQL 报错被上层 `_` 吞掉」⇒ 恒空**且零日志**。
// ⇒ 改为从**现役列**现算扩展名：原始文件名在 `text`（直传/分片/预签名/本地导入
// 四个通道都写），缺原始名时回退 `file_path` 的末段。
//
// 为什么在 Go 侧聚合扩展名：扩展名是「最后一个点之后」——SQLite 无 reverse()，
// MySQL/Postgres 各有各的写法（SUBSTRING_INDEX / SPLIT_PART+REVERSE）
// ⇒ 跨方言写 SQL 会带来「只在某个后端炸」的隐性风险（而 CI 只跑 SQLite）。
// 这里用两条**分组查询**（GROUP BY 名字/路径）把传输量压到「不同名」量级，
// 再在 Go 侧按扩展名合并。
func (r *FileCodeRepository) TopSuffixes(ctx context.Context, limit int) ([]*SuffixStat, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	agg := map[string]int64{}

	// ① 有原始名的文件分享：按原始名分组
	var named []struct {
		Name  string
		Count int64
	}
	if err := r.db().WithContext(ctx).Model(&model.FileCode{}).
		Select("text AS name, COUNT(*) AS count").
		Where("file_path <> '' AND text <> ''").
		Group("text").
		Scan(&named).Error; err != nil {
		return nil, err
	}
	for _, n := range named {
		if ext := model.FileExtension(n.Name); ext != "" {
			agg[ext] += n.Count
		}
	}

	// ② 无原始名的（部分通道不写 text）：按存储路径分组，再取末段
	var pathed []struct {
		Path  string
		Count int64
	}
	if err := r.db().WithContext(ctx).Model(&model.FileCode{}).
		Select("file_path AS path, COUNT(*) AS count").
		Where("file_path <> '' AND (text IS NULL OR text = '')").
		Group("file_path").
		Scan(&pathed).Error; err != nil {
		return nil, err
	}
	for _, p := range pathed {
		if ext := model.FileExtension(p.Path); ext != "" {
			agg[ext] += p.Count
		}
	}

	rows := make([]*SuffixStat, 0, len(agg))
	for ext, c := range agg {
		rows = append(rows, &SuffixStat{Suffix: ext, Count: c})
	}
	// 计数降序；同数按扩展名字典序 ⇒ 结果稳定（可测）
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		return rows[i].Suffix < rows[j].Suffix
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
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
//
// 条件拼装已收编 gormx/crud QuerySpec（core#88）：字段/操作符白名单
// fail-closed、Or 组自动带括号、Spec 快照隔离。条件词序与手拼时代逐条件
// 同序（EXPLAIN QUERY PLAN 逐条一致由 queriespec_pilot_test.go 锁定）。
// Unscoped（回收站开关）与健康过滤仍是 gorm 链级关注点——后者与
// CountByHealth 共用 applyHealthFilter（同一口径，勿单改）。
func (r *FileCodeRepository) ListWithFilter(ctx context.Context, q model.FileCodeQuery) ([]*model.FileCode, int64, error) {
	page, pageSize := normalizePage(q.Page, q.PageSize)

	spec := adminFileQuerySpec(q)

	query := r.db().WithContext(ctx).Model(&model.FileCode{})

	// 回收站（软删）筛选：only=仅已删 / all=含已删；默认仅存活（gorm 默认 scope）。
	// Unscoped 同时解除软删过滤，后续条件在可见全集上叠加。
	// 注意：Deleted 为空时 Unscoped 不生效 ⇒ 默认空筛选仍带软删约束
	//（fail-closed，见 queriespec_pilot_test.go）；Deleted="all" 是显式全量语义。
	switch q.Deleted {
	case "only", "all":
		query = query.Unscoped()
	}

	query = applySpecConds(query, spec)

	if q.Health != "" {
		query = applyHealthFilter(query, q.Health)
	}

	return paginate[model.FileCode](query.Order("created_at DESC"), page, pageSize)
}

// adminFileQuerySpec 构建管理端文件列表的 WHERE 条件（core#88）。
//
// 词序即执行语义：与手拼时代逐条件同序（deleted → keyword → user_id →
// upload_type → owner_ip → status → size 区间 → created 区间 → expired），
// sqlite 按该序做索引分析 ⇒ 不可随意重排。
func adminFileQuerySpec(q model.FileCodeQuery) *crud.Query {
	spec := crud.NewQuery()

	// 回收站筛选：只有条件本体进 spec；Unscoped 是 gorm 链级语义，调用方处理。
	if q.Deleted == "only" {
		spec.Where("file_codes.deleted_at", "IS NOT NULL")
	}

	if q.Keyword != "" {
		like, esc := LikeContains(q.Keyword)
		// pickup_code 同码搜索（管理端按对方口述的 6 位码定位分享）
		// prefix/suffix 死列已退役（2026-10-26，库中恒空且无写入方）。
		// 多占位符 + ESCAPE 子句片段走 WhereRaw（值仍全部占位符参数化）。
		spec.WhereRaw("code LIKE ? OR uuid_file_name LIKE ? OR text LIKE ? OR pickup_code LIKE ? OR file_path LIKE ? "+esc,
			like, like, like, like, like)
	}
	if q.UserID != nil {
		spec.Where("user_id", "=", *q.UserID)
	}
	if q.UploadType != "" {
		spec.Where("upload_type", "=", q.UploadType)
	}
	if q.OwnerIP != "" {
		spec.Where("owner_ip", "=", q.OwnerIP)
	}
	if q.Status != "" {
		spec.Where("status", "=", q.Status)
	}
	if q.MinSize != nil {
		spec.Where("size", ">=", *q.MinSize)
	}
	if q.MaxSize != nil {
		spec.Where("size", "<=", *q.MaxSize)
	}
	if q.CreatedAfter != nil {
		spec.Where("created_at", ">=", *q.CreatedAfter)
	}
	if q.CreatedBefore != nil {
		spec.Where("created_at", "<=", *q.CreatedBefore)
	}
	if q.Expired != nil {
		now := time.Now()
		if *q.Expired {
			// (A AND B) OR C：WhereGroup 组内 Or 渲染为带括号整体，OR 不击穿
			// AND 链（手拼时代靠手写括号，回归见 TestListWithFilter_ExpiredOrEscape）。
			// 组渲染词序 = Where 先于 Or，与本分支原片段词序一致。
			spec.WhereGroup(func(g *crud.Query) *crud.Query {
				return g.Where("expired_at", "IS NOT NULL").Where("expired_at", "<", now).Or("expired_count", "=", 0)
			})
		} else {
			// (A OR B) AND C：单层组的渲染是 Where 先于 Or，直接写会把 AND 叶
			// 提到 OR 组前面 ⇒ 用嵌套组保持原词序（OR 组先落、AND 叶后落）。
			spec.WhereGroup(func(g *crud.Query) *crud.Query {
				return g.WhereGroup(func(h *crud.Query) *crud.Query {
					return h.Or("expired_at", "IS NULL").Or("expired_at", ">=", now)
				}).Where("expired_count", "<>", 0)
			})
		}
	}
	return spec
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
		// 未知取值在**dao 层**保持「不过滤」（既有契约：见 filecode_health_test
		// 的「未知 health 值 = 不过滤（防御：不 panic 不误杀）」）。
		//
		// ⚠️ 但**HTTP 入口必须先校验**（core#147）：这里的「不过滤」落到接口上
		// 就是「筛选失败返回全量」——与 core#102 同族的「多给数据」。
		// 两个入口（app/admin.ParseFilesFilter、gen/handler/admin.AdminListFiles）
		// 已用 ValidHealthFilter 把未知值挡成 400 + 10001；新调用点请同样先校验。
		return query
	}
}

// ValidHealthFilter 判定 health 维度取值是否被 applyHealthFilter 认识
// （空串 = 不过滤）。core#147：applyHealthFilter 的 default 分支此前**静默丢条件**
// ⇒ 未知取值让组合过滤退化成全量 ⇒ 调用方必须先用本函数把关，未知值一律 400。
//
// ⚠️ 与 applyHealthFilter 的 switch **必须同步**（本函数就是那份清单的真相源）。
func ValidHealthFilter(s string) bool {
	switch s {
	case "", "active", "expired", "expiring_soon", "never_picked", "forever":
		return true
	}
	return false
}

// ValidDeletedFilter 判定 deleted 维度取值是否被 ListWithFilter 认识
// （空串 = 默认口径：只看未软删）。core#147：未知取值此前被当成空串处理
// ⇒ 想看回收站的人拿到的是**未删除**的行，且没有任何信号。
func ValidDeletedFilter(s string) bool {
	switch s {
	case "", "all", "only":
		return true
	}
	return false
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
