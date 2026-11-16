package dao

// gcjob.go —— 对象 GC 持久化任务的存取面（core#192）。
//
// 关键语义：
//   - EnqueueTx：与业务删除**同事务**写入（Transactional Outbox）——事务提交
//     后任务必然在表上，进程崩溃也不丢；事务回滚则任务与业务变更一起消失。
//   - ClaimDue：行级 **CAS 抢占**（认领 + 租约一步完成）。候选 = pending 且到
//     了 next_attempt_at，或 running 且租约已过期（持有者死亡/崩溃）。受影响
//     行数为 0 ⇒ 被其他 worker 抢走，换下一个候选——多副本并发认领安全。
//   - 状态机：pending → running → done / skipped（引用中跳过）/ dead
//     （attempts 耗尽，死信可重放）。失败退回 pending 且按指数退避排期。

import (
	"context"
	"errors"
	"time"

	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm"
)

// GCJobRepository gc_jobs 存取。
type GCJobRepository struct {
	conn *gorm.DB
}

// NewGCJobRepository 构造 repository。
func NewGCJobRepository(gormDB *gorm.DB) *GCJobRepository {
	return &GCJobRepository{conn: gormDB}
}

func (r *GCJobRepository) db() *gorm.DB {
	return r.conn
}

// EnqueueTx 在给定事务内写入任务（outbox 形态；调用方负责把业务删除也放进
// 同一事务）。
func (r *GCJobRepository) EnqueueTx(tx *gorm.DB, jobs []*model.GCJob) error {
	if len(jobs) == 0 {
		return nil
	}
	return tx.Create(&jobs).Error
}

// Enqueue 独立写入（非 outbox 场景：对账类任务）。
func (r *GCJobRepository) Enqueue(ctx context.Context, jobs []*model.GCJob) error {
	return r.db().WithContext(ctx).Create(&jobs).Error
}

// ClaimDue 认领一个到期任务：CAS 置 running + 续租约 + attempts+1。
// 返回 nil = 当前没有可认领任务。now/lease 由调用方注入（时钟可控，退避可测）。
func (r *GCJobRepository) ClaimDue(ctx context.Context, now time.Time, lease time.Duration) (*model.GCJob, error) {
	q := r.db().WithContext(ctx)
	for range 8 { // 抢占碰撞上限：8 轮仍抢不到按无可认领返回（下轮再来）
		var candidate model.GCJob
		err := q.Where(
			"(state = ? AND next_attempt_at <= ?) OR (state = ? AND lease_until IS NOT NULL AND lease_until < ?)",
			model.GCStatePending, now, model.GCStateRunning, now,
		).Order("next_attempt_at ASC").First(&candidate).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		leaseUntil := now.Add(lease)
		res := q.Model(&model.GCJob{}).
			Where("id = ? AND (state = ? OR (state = ? AND lease_until IS NOT NULL AND lease_until < ?))",
				candidate.ID, model.GCStatePending, model.GCStateRunning, now).
			Updates(map[string]interface{}{
				"state":           model.GCStateRunning,
				"attempts":        gorm.Expr("attempts + 1"),
				"lease_until":     &leaseUntil,
				"next_attempt_at": now,
			})
		if res.Error != nil {
			return nil, res.Error
		}
		if res.RowsAffected == 1 {
			candidate.State = model.GCStateRunning
			candidate.Attempts++
			candidate.LeaseUntil = &leaseUntil
			return &candidate, nil
		}
		// 被其他 worker 抢走 ⇒ 试下一个候选
	}
	return nil, nil
}

// MarkDone 执行成功（终态）。
func (r *GCJobRepository) MarkDone(ctx context.Context, id uint) error {
	return r.db().WithContext(ctx).Model(&model.GCJob{}).Where("id = ?", id).
		Updates(map[string]interface{}{
			"state":       model.GCStateDone,
			"lease_until": nil,
			"last_error":  "",
		}).Error
}

// MarkSkipped 引用复核命中：仍被引用 ⇒ 终态跳过（记录原因供诊断一屏可见）。
func (r *GCJobRepository) MarkSkipped(ctx context.Context, id uint, reason string) error {
	return r.db().WithContext(ctx).Model(&model.GCJob{}).Where("id = ?", id).
		Updates(map[string]interface{}{
			"state":       model.GCStateSkipped,
			"lease_until": nil,
			"last_error":  reason,
		}).Error
}

// GCBackoffPolicy 失败退避策略（worker 构造注入，测试可缩到毫秒级验证退避）。
type GCBackoffPolicy struct {
	Base        time.Duration // 首次重试延迟（指数底数）
	Cap         time.Duration // 单次延迟上限
	MaxAttempts int           // ≥ 该值即置 dead（死信）
}

// BackoffFor 第 attempts 次失败后的下一次执行延迟（指数退避，封顶 Cap）。
func (p GCBackoffPolicy) BackoffFor(attempts int) time.Duration {
	d := p.Base
	for range attempts - 1 {
		d *= 2
		if d >= p.Cap {
			return p.Cap
		}
	}
	if d <= 0 {
		d = p.Base
	}
	if d > p.Cap {
		d = p.Cap
	}
	return d
}

// MarkFailed 执行失败：attempts 未耗尽 ⇒ pending + 指数退避排期；耗尽 ⇒ dead。
func (r *GCJobRepository) MarkFailed(ctx context.Context, id uint, attempts int, lastErr string, now time.Time, p GCBackoffPolicy) error {
	if attempts >= p.MaxAttempts {
		return r.db().WithContext(ctx).Model(&model.GCJob{}).Where("id = ?", id).
			Updates(map[string]interface{}{
				"state":       model.GCStateDead,
				"lease_until": nil,
				"last_error":  lastErr,
			}).Error
	}
	next := now.Add(p.BackoffFor(attempts))
	return r.db().WithContext(ctx).Model(&model.GCJob{}).Where("id = ?", id).
		Updates(map[string]interface{}{
			"state":           model.GCStatePending,
			"lease_until":     nil,
			"next_attempt_at": next,
			"last_error":      lastErr,
		}).Error
}

// RequeueDead 死信重放：dead → pending（立即到期）。返回重放行数。
func (r *GCJobRepository) RequeueDead(ctx context.Context, now time.Time) (int64, error) {
	res := r.db().WithContext(ctx).Model(&model.GCJob{}).
		Where("state = ?", model.GCStateDead).
		Updates(map[string]interface{}{
			"state":           model.GCStatePending,
			"attempts":        0,
			"next_attempt_at": now,
			"last_error":      "",
		})
	return res.RowsAffected, res.Error
}

// CountByState 按状态计数（诊断一屏 + pb_gc_jobs 指标）。
func (r *GCJobRepository) CountByState(ctx context.Context) (map[string]int64, error) {
	var rows []struct {
		State string
		N     int64
	}
	err := r.db().WithContext(ctx).Model(&model.GCJob{}).
		Select("state, COUNT(*) AS n").Group("state").Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(rows))
	for _, row := range rows {
		out[row.State] = row.N
	}
	return out, nil
}

// OldestPendingAge 最老 pending 任务的积压时长（观测「最老多久」；无积压返回 false）。
func (r *GCJobRepository) OldestPendingAge(ctx context.Context, now time.Time) (time.Duration, bool, error) {
	var oldest model.GCJob
	err := r.db().WithContext(ctx).
		Where("state = ?", model.GCStatePending).
		Order("created_at ASC").First(&oldest).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	age := now.Sub(oldest.CreatedAt)
	if age < 0 {
		age = 0
	}
	return age, true, nil
}

// PurgeSharesAndEnqueue **Transactional Outbox**（core#192）：同一事务内完成
// 「分享行硬删（连带子行）+ gc_jobs 登记」。此前 purge 是「同步删对象 → 硬删
// 行」两段式——删库成功、删对象失败的进程崩溃窗口里，对象永久失联（远端
// 后端无对账删除 ⇒ 永久泄漏）。outbox 形态下事务提交 ⇒ 任务必然在表上，
// worker 按租约消费（崩溃/远端抖动都不丢）；事务回滚 ⇒ 行与任务一起消失，
// 不产生无主对象。
//
// jobs 由调用方（admin GCService.JobsForPaths）按 purge 目标的主/子行路径
// 预先构造；删除执行交给 worker（删前引用复核照做——同一 blob 仍被其他
// 分享引用时任务落 skipped 终态，绝不误删）。
func (r *GCJobRepository) PurgeSharesAndEnqueue(ctx context.Context, ids []uint, jobs []*model.GCJob) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var deleted int64
	err := r.conn.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := hardDeleteChildRows(tx, ids); err != nil {
			return err
		}
		res := tx.Unscoped().Where("id IN ?", ids).Delete(&model.FileCode{})
		if res.Error != nil {
			return res.Error
		}
		if err := r.EnqueueTx(tx, jobs); err != nil {
			return err
		}
		deleted = res.RowsAffected
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}
