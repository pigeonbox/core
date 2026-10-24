package dao

import (
	"context"
	"time"

	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm/clause"
)

// ==================== 取件计次（2026-10-24） ====================
//
// 语义：**一个下载令牌（取件会话）至多扣 1 次取件次数**。
// 详见 model.PickupCharge 的注释（为什么不能按「每个下载请求」计次）。
//
// 并发语义：CreatePickupCharge 走 INSERT ... ON CONFLICT DO NOTHING
// （gorm clause.OnConflict{DoNothing:true}，各方言自动生成对应 SQL）⇒
// 同一令牌并发到达时**只有一个**请求拿到「首次」，其余走「已计次」分支（不重复扣）。
// 唯一索引 = (code, token_hash) ⇒ 同一令牌对不同分享互不干扰（虽然令牌签名
// 本就绑定分享码，不同分享不可能持有同一令牌）。

// CreatePickupCharge 登记「该令牌已计次」。
//
// 返回 created=true 表示本次是**首次**用该令牌（调用方据此扣减次数）；
// created=false 表示该令牌此前已计次（同一取件会话 ⇒ 不再扣）。
// 冲突不算错误（并发重复到达是正常情形）。
//
// 登记同时落**取件人 IP** ⇒ owner 侧的逐条取件明细有据可查（此前只有
// file_codes 的「最后一次 IP + 计数」，看不到逐条）。
func (r *FileCodeRepository) CreatePickupCharge(ctx context.Context, code, tokenHash, channel, ip string) (bool, error) {
	if tokenHash == "" {
		// 无令牌指纹（令牌未启用/未携带）⇒ 不做会话级去重，按「每次都算新取件」处理。
		return true, nil
	}
	row := &model.PickupCharge{
		Code:      code,
		TokenHash: tokenHash,
		Channel:   channel,
		IP:        ip,
		ChargedAt: time.Now(),
	}
	res := r.db().WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(row)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// DeletePickupCharge 回滚计次登记（扣次失败时用：登记了但没真扣 ⇒ 必须撤销，
// 否则该令牌会被误判成「已计次」，后续重试将免费放行）。
//
// ⚠️ **硬删**（Unscoped）：计次表带 (code, token_hash) 唯一索引 ⇒ 软删的行仍占
// 索引 ⇒ 同一令牌再次登记会被唯一约束挡掉（表现为「已计次」⇒ 额度用完后
// 该令牌被误判成免费放行）。计次记录本就是短命数据，不需要软删语义。
func (r *FileCodeRepository) DeletePickupCharge(ctx context.Context, code, tokenHash string) error {
	if tokenHash == "" {
		return nil
	}
	return r.db().WithContext(ctx).
		Unscoped().
		Where("code = ? AND token_hash = ?", code, tokenHash).
		Delete(&model.PickupCharge{}).Error
}

// DeleteExpiredPickupCharges 清理超过留存期的取件记录（0 = 永久保留）。
//
// ⚠️ 留存期**不能**跟着令牌 TTL 走（2026-10-24 缺陷）：这张表同时是
// ①「同一令牌不重复计次」的去重键（令牌过期后即可丢）与 ② owner 侧「取件历史」
// 的逐条明细（IP/时间/交付面，要留很久）⇒ 清理窗口若取 TTL+1h，owner 看到的明细
// **最多只存在 1 小时** ⇒ 逐条取件历史形同虚设。
//
// 同样**硬删**（Unscoped）：软删行仍占唯一索引 ⇒ 同一令牌无法再登记。
func (r *FileCodeRepository) DeleteExpiredPickupCharges(ctx context.Context, before time.Time) (int64, error) {
	if before.IsZero() {
		return 0, nil // 0 = 永久保留
	}
	res := r.db().WithContext(ctx).
		Unscoped().
		Where("charged_at < ?", before).
		Delete(&model.PickupCharge{})
	if res.Error != nil {
		return 0, res.Error
	}
	return res.RowsAffected, nil
}

// DeleteOrphanPickupCharges 清理「分享已不存在」的孤儿明细（分享被硬删后，
// 其取件明细留着既占空间、又没有主人可看）。软删（回收站）不算孤儿。
func (r *FileCodeRepository) DeleteOrphanPickupCharges(ctx context.Context) (int64, error) {
	res := r.db().WithContext(ctx).
		Unscoped().
		Where("code NOT IN (SELECT code FROM file_codes WHERE deleted_at IS NULL)").
		Delete(&model.PickupCharge{})
	if res.Error != nil {
		return 0, res.Error
	}
	return res.RowsAffected, nil
}

// HasPickupCharge 判「该取件会话是否已计次」。
//
// 用途：取件页在**次数已用完**时的放行判定 —— 已经取过的人（如文本分享的
// 正文在取件页交付）应当仍能再打开看（他不重复扣次），而不是被一刀切拒绝。
func (r *FileCodeRepository) HasPickupCharge(ctx context.Context, code, sessionKey string) (bool, error) {
	if sessionKey == "" {
		return false, nil
	}
	var n int64
	err := r.db().WithContext(ctx).
		Model(&model.PickupCharge{}).
		Where("code = ? AND token_hash = ?", code, sessionKey).
		Count(&n).Error
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ListPickups 取某分享的逐条取件明细（最近 N 条，按时间倒序）。
//
// owner 侧「取件历史」的数据源：**一次取件 = 一行**（多文件逐个下载/断点续传
// 属同一次取件会话 ⇒ 只有一行，不会把「一次取件」显示成 N 次）。
func (r *FileCodeRepository) ListPickups(ctx context.Context, code string, limit int) ([]model.PickupCharge, error) {
	if limit <= 0 {
		limit = 20
	}
	var rows []model.PickupCharge
	err := r.db().WithContext(ctx).
		Where("code = ?", code).
		Order("charged_at DESC, id DESC").
		Limit(limit).
		Find(&rows).Error
	return rows, err
}

// ChargePickupOnce 「按取件会话」计次：同一令牌至多扣 1 次取件次数。
//
// 返回：
//   - charged=true：本次真正扣了次数（首次用该令牌）；
//   - exhausted=true：取件次数已用完且本令牌尚未计次 ⇒ 拒绝交付（调用方回 10003）；
//   - charged=false, exhausted=false：同一令牌已计次 ⇒ 本次不再扣（同一次取件内
//     的后续下载：多文件逐个下载 / 断点续传 / 重复点击）。
//
// 顺序：先登记（唯一索引裁决「首次」），再原子扣减；扣减失败（次数耗尽或 DB 错）
// ⇒ 撤销登记，让后续重试仍是一次「新取件」（不因一次失败把额度白扔）。
func (r *FileCodeRepository) ChargePickupOnce(ctx context.Context, code, tokenHash, channel, ip string) (charged bool, exhausted bool, err error) {
	created, err := r.CreatePickupCharge(ctx, code, tokenHash, channel, ip)
	if err != nil {
		return false, false, err
	}
	if !created {
		// 同一取件会话已计次 ⇒ 本次不重复扣。
		return false, false, nil
	}
	ok, err := r.DecrementExpiredCount(ctx, code)
	if err != nil {
		_ = r.DeletePickupCharge(ctx, code, tokenHash)
		return false, false, err
	}
	if !ok {
		// 次数已耗尽（WHERE expired_count>0 未命中）⇒ 撤销登记。
		_ = r.DeletePickupCharge(ctx, code, tokenHash)
		return false, true, nil
	}
	return true, false, nil
}
