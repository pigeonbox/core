package admin

// gcjob.go —— 对象 GC 任务的执行器（core#192）。
//
// 与 janitor（对账）的分工：janitor 扫全量差集（低频、report-only 于远端）；
// GC 任务面消费**精确知道要删哪个对象**的持久化任务（Transactional Outbox
// 产生，purge 路径同事务登记）。执行器两条底线：
//
//  1. **删前引用复核**：按对象路径查 file_codes / file_code_files 引用（含软删
//     行——回收站行物理留存），仍被引用 ⇒ done 侧的 skipped 终态。这是
//     「允许延迟，绝不误删」的底线（同一 blob 被多 share 复用、入队后引用
//     关系变化等都拦在这里）。
//  2. **租约与退避**：ClaimDue 的 CAS 保证多副本/多轮次只有一个执行者；
//     远端 5xx/限流按指数退避重试，attempts 耗尽进 dead（死信），管理端
//     可见可重放。

import (
	"context"
	"fmt"
	"time"

	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/pkg/metrics"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// GCObjectDeleter GC 执行器所需的最窄存储能力。
type GCObjectDeleter interface {
	DeleteFile(ctx context.Context, relPath string) error
}

// gcBatchLimit 单轮认领上限：与 janitor 同节奏（低频循环），防一次积压风暴
// 占住整个轮次。剩余由下一轮继续（任务在表上，不丢）。
const gcBatchLimit = 100

// GCService 对象 GC 任务执行器。
type GCService struct {
	repo         *dao.GCJobRepository
	fileCodeRepo *dao.FileCodeRepository
	storage      GCObjectDeleter

	backoff     dao.GCBackoffPolicy
	leaseFor    time.Duration
	nowFn       func() time.Time
	backendOf   func() string
	skipRecheck bool // 测试注入：跳过引用复核（默认恒开——生产不可关）
}

// GCServiceOption 执行器选项。
type GCServiceOption func(*GCService)

// WithGCClock 时钟注入（测试退避/租约用）。
func WithGCClock(now func() time.Time) GCServiceOption {
	return func(s *GCService) { s.nowFn = now }
}

// WithGCBackoff 退避策略注入（测试缩到毫秒级）。
func WithGCBackoff(p dao.GCBackoffPolicy) GCServiceOption {
	return func(s *GCService) { s.backoff = p }
}

// WithGCLease 租约时长（默认 10 分钟：远端慢删不轻易被抢；持有者崩溃 ⇒
// 至多 10 分钟后任务被其他轮次接管）。
func WithGCLease(d time.Duration) GCServiceOption {
	return func(s *GCService) { s.leaseFor = d }
}

// WithGCBackendType 入队后端类型来源（观测用；nil = 空）。
func WithGCBackendType(fn func() string) GCServiceOption {
	return func(s *GCService) { s.backendOf = fn }
}

// NewGCService 构造 GC 执行器。storage 为 nil 时（未装配）RunOnce 空转。
func NewGCService(gormDB *gorm.DB, storage GCObjectDeleter, opts ...GCServiceOption) *GCService {
	s := &GCService{
		repo:         dao.NewGCJobRepository(gormDB),
		fileCodeRepo: dao.NewFileCodeRepository(gormDB),
		storage:      storage,
		backoff: dao.GCBackoffPolicy{
			Base:        time.Minute,
			Cap:         time.Hour,
			MaxAttempts: 5,
		},
		leaseFor: 10 * time.Minute,
		nowFn:    time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// SetDeleter 注入对象删除能力（SetStorage 同实例转注；nil 安全）。
func (s *GCService) SetDeleter(d GCObjectDeleter) { s.storage = d }

// gcJobFor 构造一条待入队任务（ObjectKey 非空由调用方保证）。
func (s *GCService) gcJobFor(objectKey, reason string) *model.GCJob {
	backend := ""
	if s.backendOf != nil {
		backend = s.backendOf()
	}
	now := s.nowFn()
	return &model.GCJob{
		ObjectKey:     objectKey,
		Backend:       backend,
		Reason:        reason,
		State:         model.GCStatePending,
		NextAttemptAt: now,
	}
}

// JobsForPaths 把一组对象路径转成待入队任务（outbox 事务内经
// GCJobRepository.EnqueueTx 落表）。
func (s *GCService) JobsForPaths(paths []string, reason string) []*model.GCJob {
	jobs := make([]*model.GCJob, 0, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		jobs = append(jobs, s.gcJobFor(p, reason))
	}
	return jobs
}

// EnqueuePurge outbox 提交（core#192）：同一事务内「硬删分享行（连带子行）+
// gc_jobs 登记」。purge 路径入口；paths=主/子行全部对象路径（Unscoped 口径）。
func (s *GCService) EnqueuePurge(ctx context.Context, ids []uint, paths []string) (int64, error) {
	return s.repo.PurgeSharesAndEnqueue(ctx, ids, s.JobsForPaths(paths, model.GCReasonPurge))
}

// RunOnce 消费一轮到期任务（认领 → 复核 → 删 → 终态），返回处理条数。
// panic 不外抛（bootstrap 循环有收编，这里保函数纯粹）。
func (s *GCService) RunOnce(ctx context.Context) (int, error) {
	if s.storage == nil {
		return 0, nil
	}
	processed := 0
	for processed < gcBatchLimit {
		job, err := s.repo.ClaimDue(ctx, s.nowFn(), s.leaseFor)
		if err != nil {
			return processed, fmt.Errorf("认领 GC 任务失败: %w", err)
		}
		if job == nil {
			break
		}
		s.execute(ctx, job)
		processed++
	}
	s.recordMetrics(ctx)
	return processed, nil
}

// execute 单任务执行：引用复核 → 删对象 → 终态。复核查不清按失败退避重试
// （宁可延迟，不可误删）。
func (s *GCService) execute(ctx context.Context, job *model.GCJob) {
	if job.ObjectKey == "" {
		_ = s.repo.MarkSkipped(ctx, job.ID, "empty object_key")
		return
	}
	// 删前引用复核（底线，恒开）：软删行也计——回收站行的物理文件留存供恢复。
	if !s.skipRecheck {
		refs, err := s.fileCodeRepo.CountFilePathReferences(ctx, job.ObjectKey, 0)
		if err != nil {
			s.fail(ctx, job, fmt.Sprintf("reference check failed: %v", err))
			return
		}
		if refs > 0 {
			if serr := s.repo.MarkSkipped(ctx, job.ID, fmt.Sprintf("referenced by %d share rows, skip", refs)); serr != nil {
				logger.CtxWarn(ctx, "mark gc job skipped failed", zap.Uint("id", job.ID), zap.Error(serr))
			}
			return
		}
	}
	if err := s.storage.DeleteFile(ctx, job.ObjectKey); err != nil {
		s.fail(ctx, job, err.Error())
		return
	}
	if derr := s.repo.MarkDone(ctx, job.ID); derr != nil {
		logger.CtxWarn(ctx, "mark gc job done failed", zap.Uint("id", job.ID), zap.Error(derr))
	}
}

// fail 记一次失败：退避排期或进死信。
func (s *GCService) fail(ctx context.Context, job *model.GCJob, reason string) {
	err := s.repo.MarkFailed(ctx, job.ID, job.Attempts, reason, s.nowFn(), s.backoff)
	if err != nil {
		logger.CtxWarn(ctx, "mark gc job failed-state failed",
			zap.Uint("id", job.ID), zap.Int("attempts", job.Attempts), zap.Error(err))
	}
}

// recordMetrics 每轮末刷新 pb_gc_jobs{state} 仪表（积压一屏可见）。
func (s *GCService) recordMetrics(ctx context.Context) {
	counts, err := s.repo.CountByState(ctx)
	if err != nil {
		logger.CtxWarn(ctx, "gc job counts failed", zap.Error(err))
		return
	}
	metrics.RecordGCJobs(counts)
}
