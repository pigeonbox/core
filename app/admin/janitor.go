// janitor.go 存储对账与日志保留（治理 2026-10-03）。
//
// 此前代码注释多次声称"孤儿文件由清理任务兜底"但该任务并不存在：
// 物理文件在 DB 无引用（写库失败/分片中断/老 bug 遗留）时永不回收。
// 本文件补上两个低频后台任务：
//   - ReconcileOrphans：物理文件 vs DB 引用差集清理。local 后端真删除；
//     远端后端（s3/webdav/多云）只统计报告不删除（远端误删代价不对称，
//     且全量 List 成本高，等真实需求出现再做远端对账）。
//   - CleanupLogs：transfer_log / admin_operation_log 按保留天数清理
//     （此前审计表无限膨胀）。
package admin

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/storage"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// StorageProvider janitor 所需存储能力：通用操作 + 本地后端判定/根目录。
// EffectiveType/DataPath 是 *storage.StorageService 的扩展能力（不在
// StorageInterface 内），按消费侧窄接口收窄依赖，bootstrap 传具体实现即可。
type StorageProvider interface {
	storage.StorageInterface
	EffectiveType() storage.StorageType
	DataPath() string
	// ListRemoteObjects 远端对象列举（presign 孤儿清理；本地后端返回错误）。
	ListRemoteObjects(ctx context.Context, prefix string) ([]storage.RemoteObjectInfo, error)
}

// Janitor 存储对账 + 日志保留。svc 为 nil 或非 local 时对账自动跳过。
type Janitor struct {
	svc           StorageProvider
	retentionDays int // 0 = 永久保留日志

	fileCodeRepo *dao.FileCodeRepository
	fileFileRepo *dao.FileCodeFileRepository
	chunkRepo    *dao.ChunkRepository
	transferRepo *dao.TransferLogRepository
	adminLogRepo *dao.AdminOperationLogRepository
}

// NewJanitor 创建 janitor。
func NewJanitor(gormDB *gorm.DB, svc StorageProvider, retentionDays int) *Janitor {
	return &Janitor{
		svc:           svc,
		retentionDays: retentionDays,
		fileCodeRepo:  dao.NewFileCodeRepository(gormDB),
		fileFileRepo:  dao.NewFileCodeFileRepository(gormDB),
		chunkRepo:     dao.NewChunkRepository(gormDB),
		transferRepo:  dao.NewTransferLogRepository(gormDB),
		adminLogRepo:  dao.NewAdminOperationLogRepository(gormDB),
	}
}

// ReconcileOrphans 物理文件对账。返回 (扫描文件数, 清理文件数, error)。
func (j *Janitor) ReconcileOrphans(ctx context.Context) (int, int, error) {
	if j.svc == nil {
		return 0, 0, fmt.Errorf("存储实例未就绪")
	}
	if j.svc.EffectiveType() != storage.StorageTypeLocal {
		logger.Info("orphan reconcile skipped: non-local backend (report-only policy)")
		return 0, 0, nil
	}
	root := j.svc.DataPath()
	if root == "" {
		return 0, 0, fmt.Errorf("存储根目录未配置")
	}

	// 1. 收集 DB 引用集（含软删记录——可恢复的分享其物理文件不算孤儿）
	rows, err := j.fileCodeRepo.ListAllIncludingDeleted(ctx)
	if err != nil {
		return 0, 0, err
	}
	referenced := make(map[string]bool, len(rows))
	for _, r := range rows {
		if p := r.GetFilePath(); p != "" {
			referenced[filepath.ToSlash(filepath.Clean(p))] = true
		}
	}
	// 子表路径（2026-10-12 修复）：多文件分享的非首文件只存在于 file_code_files，
	// 引用集漏掉会被对账误当孤儿删除。取集失败直接放弃本轮（引用集不全=误删风险）
	childPaths, cerr := j.fileFileRepo.ListAllPathsIncludingDeleted(ctx)
	if cerr != nil {
		return 0, 0, cerr
	}
	for _, p := range childPaths {
		referenced[filepath.ToSlash(filepath.Clean(p))] = true
	}

	// 2. 收集分片会话集合
	uploadIDs, err := j.chunkRepo.ListSessionIDs(ctx)
	if err != nil {
		return 0, 0, err
	}
	activeChunks := make(map[string]bool, len(uploadIDs))
	for _, id := range uploadIDs {
		activeChunks[id] = true
	}

	// 3. 遍历物理目录（只看 uploads/ 与 chunks/ 两个受管子树）。
	// mtime 宽限期（默认 24h）：在途 presign 直传/新建分片文件的 DB 引用要等
	// Complete 才落库，先取快照再扫盘存在竞态窗口——只清理"无引用且足够老"的
	// 文件，正在写入/刚写完的文件一律放过（回归：曾误删在途 presign 直传）。
	grace := 24 * time.Hour
	olderThan := time.Now().Add(-grace)
	scanned, removed := 0, 0
	cleanFile := func(path string) {
		scanned++
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return
		}
		rel = filepath.ToSlash(filepath.Clean(rel))
		if referenced[rel] {
			return
		}
		if fi, serr := os.Stat(path); serr == nil && fi.ModTime().After(olderThan) {
			return // 太新，可能是正在上传的文件
		}
		if err := j.svc.DeleteFile(ctx, rel); err != nil {
			logger.Warn("orphan file delete failed", zap.String("path", rel), zap.Error(err))
			return
		}
		removed++
	}

	uploadsDir := filepath.Join(root, "uploads")
	if entries, err := os.Stat(uploadsDir); err == nil && entries.IsDir() {
		_ = filepath.WalkDir(uploadsDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			cleanFile(path)
			return nil
		})
	}

	// 分片目录：chunks/<uploadID> 整目录粒度对账
	chunksDir := filepath.Join(root, "chunks")
	if entries, err := os.ReadDir(chunksDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() || activeChunks[e.Name()] {
				continue
			}
			dir := filepath.Join(chunksDir, e.Name())
			if fi, serr := os.Stat(dir); serr == nil && fi.ModTime().After(olderThan) {
				continue // 太新，可能是活跃会话
			}
			if err := os.RemoveAll(dir); err != nil {
				logger.Warn("orphan chunk dir delete failed", zap.String("dir", dir), zap.Error(err))
				continue
			}
			removed++
		}
	}

	if removed > 0 {
		logger.Info("orphan reconcile done",
			zap.Int("scanned", scanned), zap.Int("removed", removed))
	}
	return scanned, removed, nil
}

// CleanupLogs 按保留天数清理传输日志与审计日志。返回删除行数。
func (j *Janitor) CleanupLogs(ctx context.Context) (int64, error) {
	if j.retentionDays <= 0 {
		return 0, nil // 0 = 永久保留
	}
	cutoff := time.Now().AddDate(0, 0, -j.retentionDays)
	var total int64
	n, err := j.transferRepo.DeleteOlderThan(ctx, cutoff)
	if err != nil {
		return total, err
	}
	total += n
	if n, err = j.adminLogRepo.DeleteOlderThan(ctx, cutoff); err != nil {
		return total, err
	}
	total += n
	if total > 0 {
		logger.Info("log retention cleanup done",
			zap.Int64("deleted", total), zap.Int("retention_days", j.retentionDays))
	}
	return total, nil
}

// CleanupPickupCharges 清理取件记录（2026-10-24）。
//
// 这张表有两个角色：**去重键**（同一令牌不重复计次，令牌过期后即可丢）与
// **owner 侧取件历史的逐条明细**（IP/时间/交付面，要留很久）。
// ⇒ 清理窗口**必须按明细的留存需求**给（默认 30 天），不能跟着令牌 TTL 走
// （此前取 TTL+1h ⇒ owner 看到的明细最多存在 1 小时 ⇒ 逐条历史形同虚设）。
//
// retentionDays = 0 ⇒ 永久保留（不按时间删）。
//
// 另清「孤儿」：分享被硬删后其明细没有主人可看 ⇒ 一并删除（软删/回收站不算）。
func (j *Janitor) CleanupPickupCharges(ctx context.Context, retentionDays int) (int64, error) {
	var total int64
	if retentionDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -retentionDays)
		n, err := j.fileCodeRepo.DeleteExpiredPickupCharges(ctx, cutoff)
		if err != nil {
			return 0, err
		}
		total += n
	}
	orphan, err := j.fileCodeRepo.DeleteOrphanPickupCharges(ctx)
	if err != nil {
		return total, err
	}
	total += orphan
	if total > 0 {
		logger.Info("pickup record cleanup done",
			zap.Int64("deleted", total), zap.Int("retention_days", retentionDays))
	}
	return total, nil
}

// CleanupStaleUploads 清理弃管的分片会话（治理 2026-10-03）：
// init 后永不 complete 的会话此前永久滞留（目录 + DB 行），且唯一的
// 手动清理在非 SQLite 库上直接 SQL 报错。incomplete 超龄 → 删目录 + 删行；
// 已完成会话旧行单独清理（防 upload_chunks 表无限膨胀）。
func (j *Janitor) CleanupStaleUploads(ctx context.Context, maxAge time.Duration) (dirsRemoved int, err error) {
	repo := j.chunkRepo

	stale, err := repo.GetIncompleteUploads(ctx, maxAge)
	if err != nil {
		return 0, err
	}
	for _, c := range stale {
		if j.svc != nil {
			if cerr := j.svc.CleanChunks(ctx, c.UploadID); cerr != nil {
				logger.Warn("stale chunk dir cleanup failed", zap.String("upload_id", c.UploadID), zap.Error(cerr))
			} else {
				dirsRemoved++
			}
		}
	}
	if _, err := repo.DeleteStaleSessions(ctx, maxAge, false); err != nil {
		return dirsRemoved, err
	}
	// 已完成会话的旧行（7 天）只删 DB 行，物理目录已在合并成功时清理
	if _, err := repo.DeleteStaleSessions(ctx, 7*24*time.Hour, true); err != nil {
		return dirsRemoved, err
	}
	if len(stale) > 0 {
		logger.Info("stale chunk sessions cleaned",
			zap.Int("sessions", len(stale)), zap.Int("dirs_removed", dirsRemoved))
	}
	return dirsRemoved, nil
}

// CleanRemotePresignOrphans 清理远端后端的 presign 孤儿对象：
// Init 后未 Complete 的直传残留（对象 key 形如 uploads/日期/up_<uuid>.ext，
// basename up_ 前缀与普通上传的 uuid 名可区分），无 DB 引用且超过宽限期的删除。
// 远端后端此前完全不对账（ReconcileOrphans 对远端只报告），孤儿只增不减——
// 且匿名 presign Init 可被滥用为免费网盘（2026-10-06 审计确认），此方法为其兜底。
// 本地后端不适用（ReconcileOrphans 磁盘对账已覆盖）。
func (j *Janitor) CleanRemotePresignOrphans(ctx context.Context, olderThan time.Duration) (int, error) {
	keys, err := j.ScanRemotePresignOrphans(ctx, olderThan)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, key := range keys {
		if err := j.svc.DeleteFile(ctx, key); err != nil {
			logger.CtxWarn(ctx, "remote presign orphan delete failed", "key", key, "err", err)
			continue
		}
		removed++
	}
	if removed > 0 {
		logger.Info("remote presign orphan objects removed", zap.Int("count", removed))
	}
	return removed, nil
}

// CountRemotePresignOrphans 只读统计：当前满足清理判定的 presign 孤儿数
// （存储洞察 /admin/storage/insights 用，不删除任何对象）。
func (j *Janitor) CountRemotePresignOrphans(ctx context.Context, olderThan time.Duration) (int, error) {
	keys, err := j.ScanRemotePresignOrphans(ctx, olderThan)
	if err != nil {
		return 0, err
	}
	return len(keys), nil
}

// ScanRemotePresignOrphans 列举满足清理判定的远端 presign 孤儿对象 key（只读）：
// up_ 前缀 + 无 DB 引用 + 超过 olderThan 宽限期。本地后端返回空集
// （磁盘对账 ReconcileOrphans 已覆盖，无 presign 孤儿语义）。
func (j *Janitor) ScanRemotePresignOrphans(ctx context.Context, olderThan time.Duration) ([]string, error) {
	if j.svc == nil {
		return nil, fmt.Errorf("存储实例未就绪")
	}
	if j.svc.EffectiveType() == storage.StorageTypeLocal {
		return nil, nil // 本地由 ReconcileOrphans 覆盖
	}
	lister, ok := j.svc.(interface {
		ListRemoteObjects(ctx context.Context, prefix string) ([]storage.RemoteObjectInfo, error)
	})
	if !ok {
		return nil, fmt.Errorf("存储实例不支持远端列举")
	}

	objs, err := lister.ListRemoteObjects(ctx, "uploads/")
	if err != nil {
		return nil, err
	}

	// 引用集：file_codes.file_path（含软删行——与 ReconcileOrphans 同口径）。
	// presign 孤儿 key 的 basename 恒为 up_<uuid>.<ext>，不可能出现在合法引用里
	//（uuid v4 字符集不含 u/p），故 up_ 前缀+无引用 即可判定孤儿。
	referenced := make(map[string]bool)
	rows, err := j.fileCodeRepo.ListAllIncludingDeleted(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if p := r.GetFilePath(); p != "" {
			referenced[filepath.ToSlash(filepath.Clean(p))] = true
		}
	}

	cutoff := time.Now().Add(-olderThan)
	var orphanKeys []string
	for _, o := range objs {
		base := path.Base(o.Key)
		if !strings.HasPrefix(base, "up_") {
			continue
		}
		if referenced[strings.TrimSuffix(o.Key, "/")] {
			continue
		}
		if o.ModTime.After(cutoff) {
			continue // 宽限期内（可能正在直传）
		}
		orphanKeys = append(orphanKeys, o.Key)
	}
	return orphanKeys, nil
}
