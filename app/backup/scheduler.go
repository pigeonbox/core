package backup

// 定时备份调度器（2026-11-06，5.1.0）：把「导出 + 落盘 + 保留」串成一次可重入
// 的操作，并把状态暴露给管理面/诊断。

// # 三条硬约束（每条都对应一次真实的错误方向）
//
//  1. **先写临时文件、成功后改名**：半个包不能出现在「今天已备份」的判据里，
//     否则磁盘满的那一天会被自己的残骸记成「备份成功」。
//  2. **只有成功才做保留清理**：失败之后删旧包＝把灾备变成事故放大器
//     （本来就没了新包，还把旧包删了）。
//  3. **单飞**：定时任务与「立即备份」按钮可能同时到 ⇒ 并发导出同一实例会把
//     存储读两遍、把目录写两份，还可能互相删对方的临时文件。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oekit/concurrency/async"
	"go.uber.org/zap"

	"github.com/pigeonbox/core/pkg/logger"
)

// ErrBusy 已有备份任务在执行（并发导出无意义，直接拒绝）。
var ErrBusy = fmt.Errorf("已有备份任务在执行")

// SchedulerConfig 调度器配置（来自 conf.BackupConfig，已解析成绝对目录）。
type SchedulerConfig struct {
	// Dir 备份包落盘目录（**绝对路径**；调用方负责解析默认值）。
	Dir string
	// Keep 保留份数，`<=0 = 不限`。
	Keep int
	// At 每日时刻（`HH:MM`，空串取 03:00）。
	At string
}

// RunResult 一次备份的结果（给管理面/日志用）。
type RunResult struct {
	// Path 最终包路径（临时文件改名后的位置）。
	Path string
	// Name 包名。
	Name string
	// Size 包字节数。
	Size int64
	// ObjectCount / ObjectBytes 包内对象数与总字节（来自 manifest）。
	ObjectCount int64
	ObjectBytes int64
	// FileCount 包内分享/取件码记录数。
	FileCount int64
	// MissingCount DB 引用了但磁盘上已不存在的对象数（**如实记账**，不算失败）。
	MissingCount int
	// Pruned 本次被保留策略删除的旧包名。
	Pruned []string
	// PruneErrors 删除旧包时的错误（不影响本次备份成功，但要能看见）。
	PruneErrors []string
	// StartedAt / Duration 耗时。
	StartedAt time.Time
	Duration  time.Duration
}

// Scheduler 定时备份调度器（进程内单例，由 bootstrap 装配）。
type Scheduler struct {
	svc  *Service
	dir  string
	keep int
	at   DailyAt

	// now 可注入（单测断言「到期判定」时不必等真实时间）。
	now func() time.Time

	// runMu 单飞：TryLock 失败即「正在跑」。
	runMu sync.Mutex
	// stateMu 只保护下面的状态字段 ⇒ 长备份**不阻塞**状态查询
	// （管理台轮询 /pb doctor 不该被几十 GB 的导出卡住）。
	stateMu  sync.RWMutex
	running  bool
	lastRun  time.Time
	lastFile string
	lastSize int64
	lastErr  string
}

// NewScheduler 创建调度器。
//
// Dir 必须非空且**绝对**：相对路径按进程 CWD 解析，而 NAS 包 / 容器 / systemd
// 的 CWD 都不指向数据目录 ⇒ 备份写到莫名的地方或直接 EACCES（同 preview 缓存
// 教训）。这里显式拒绝，而不是替调用方猜。
func NewScheduler(svc *Service, cfg SchedulerConfig) (*Scheduler, error) {
	if svc == nil {
		return nil, fmt.Errorf("备份服务未就绪")
	}
	dir := filepath.Clean(strings.TrimSpace(cfg.Dir))
	if dir == "" || dir == "." {
		return nil, fmt.Errorf("备份目录未配置")
	}
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("备份目录必须是绝对路径（相对路径按进程 CWD 解析，部署形态一变就写错地方）: %q", cfg.Dir)
	}
	at, err := ParseDailyAt(cfg.At)
	if err != nil {
		return nil, err
	}
	return &Scheduler{svc: svc, dir: dir, keep: cfg.Keep, at: at, now: time.Now}, nil
}

// Dir 备份目录（绝对路径）。
func (s *Scheduler) Dir() string { return s.dir }

// Keep 保留份数（`<=0 = 不限`）。
func (s *Scheduler) Keep() int { return s.keep }

// At 每日时刻。
func (s *Scheduler) At() DailyAt { return s.at }

// Now 当前时钟（单测可替换）。
func (s *Scheduler) Now() time.Time { return s.now() }

// List 列出已有备份包（新→旧）。
func (s *Scheduler) List() ([]FileInfo, error) { return ListFiles(s.dir) }

// Due 现在是否该备份（目标时刻已过 + 当天没有包）。
func (s *Scheduler) Due() (bool, error) {
	files, err := ListFiles(s.dir)
	if err != nil {
		return false, err
	}
	now := s.now()
	return s.at.Due(now, HasBackupForDate(files, DateKey(now))), nil
}

// RunIfDue 到期才跑（定时循环用）。未到期返回 (nil, nil)。
func (s *Scheduler) RunIfDue(ctx context.Context) (*RunResult, error) {
	due, err := s.Due()
	if err != nil || !due {
		return nil, err
	}
	return s.RunOnce(ctx)
}

// RunOnce 立刻执行一次备份（不受时刻判定约束；「立即备份」按钮与到期补跑
// 共用这一条路径）。
//
// 失败语义：文件系统层面失败（磁盘满、权限、目录不可写）**整体报错且不清理
// 任何旧包**；对象缺失（DB 有引用、磁盘上没有）不是失败——它进 manifest 的
// missing_objects，同样的账在离线导出里也这么记。
func (s *Scheduler) RunOnce(ctx context.Context) (*RunResult, error) {
	if !s.runMu.TryLock() {
		return nil, ErrBusy
	}
	defer s.runMu.Unlock()
	return s.runLocked(ctx)
}

// Start 在**后台**启动一次备份（管理台「立即备份」按钮用）。
//
// 为什么不走 RunOnce：一次导出可能要几分钟到几十分钟（几十 GB 对象），HTTP
// 请求等它必然被反代/网关掐断，用户看到的是「失败」，而备份其实还在写。
// ⇒ 只回答「已开始 / 已在跑」，进度与结果从 GET /admin/backups 读。
//
// 复用同一把 runMu ⇒ 与定时任务互斥，不会两份同时导出。
func (s *Scheduler) Start(ctx context.Context) error {
	if !s.runMu.TryLock() {
		return ErrBusy
	}
	async.GoSafe(func() {
		defer s.runMu.Unlock()
		// 错误已在 runLocked 内部落进状态（可查询），这里只留一条日志。
		_, _ = s.runLocked(ctx)
	})
	return nil
}

// runLocked 真正干活的那段（调用方必须已持有 runMu）。
func (s *Scheduler) runLocked(ctx context.Context) (*RunResult, error) {
	started := s.now()
	s.setRunning(true)
	s.setError("")
	defer s.setRunning(false)

	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		err = fmt.Errorf("创建备份目录失败: %w", err)
		s.recordFailure(started, err)
		return nil, err
	}

	// 文件名里的时间 = **本次开始时刻**（写进名字后不再变，与 mtime 无关）。
	name := FileName(started)
	finalPath := filepath.Join(s.dir, name)
	tmpPath := finalPath + PartialSuffix

	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		err = fmt.Errorf("创建临时备份文件失败: %w", err)
		s.recordFailure(started, err)
		return nil, err
	}

	manifest, exportErr := s.svc.Export(ctx, tmp)
	if exportErr == nil {
		// 先落盘再改名：Sync 失败（磁盘满）也要按失败处理，不能靠「文件已存在」
		// 冒充成功。
		exportErr = tmp.Sync()
	}
	if closeErr := tmp.Close(); exportErr == nil && closeErr != nil {
		exportErr = closeErr
	}
	if exportErr != nil {
		err = fmt.Errorf("导出备份失败: %w", exportErr)
		// 删掉残骸：它既不是可用的包，也不该占据任何空间。
		if rmErr := os.Remove(tmpPath); rmErr != nil && !os.IsNotExist(rmErr) {
			logger.Warn("备份临时文件清理失败", zap.String("path", tmpPath), zap.Error(rmErr))
		}
		s.recordFailure(started, err)
		return nil, err
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		err = fmt.Errorf("备份包改名失败: %w", err)
		_ = os.Remove(tmpPath)
		s.recordFailure(started, err)
		return nil, err
	}

	result := &RunResult{Path: finalPath, Name: name, StartedAt: started}
	if info, statErr := os.Stat(finalPath); statErr == nil {
		result.Size = info.Size()
	}
	if manifest != nil {
		result.ObjectCount = manifest.ObjectCount
		result.ObjectBytes = manifest.ObjectBytes
		result.FileCount = manifest.FileCount
		result.MissingCount = len(manifest.MissingObjects)
	}

	// 保留清理：**只在成功之后**。删除失败只告警——旧包留着不伤，误删才是事故。
	result.Pruned, result.PruneErrors = s.prune()
	result.Duration = s.now().Sub(started)

	s.stateMu.Lock()
	s.lastRun = started
	s.lastFile = name
	s.lastSize = result.Size
	s.stateMu.Unlock()

	logger.Info("auto backup done",
		zap.String("file", name),
		zap.Int64("size", result.Size),
		zap.Int64("objects", result.ObjectCount),
		zap.Int("missing", result.MissingCount),
		zap.Int("pruned", len(result.Pruned)),
		zap.Duration("duration", result.Duration))
	return result, nil
}

// prune 按保留策略删旧包（返回删掉的名字与失败信息）。
func (s *Scheduler) prune() ([]string, []string) {
	files, err := ListFiles(s.dir)
	if err != nil {
		return nil, []string{err.Error()}
	}
	toDelete := SelectForRetention(files, s.keep)
	if len(toDelete) == 0 {
		return nil, nil
	}
	var deleted, failed []string
	for _, f := range toDelete {
		if err := os.Remove(filepath.Join(s.dir, f.Name)); err != nil {
			failed = append(failed, f.Name+": "+err.Error())
			continue
		}
		deleted = append(deleted, f.Name)
	}
	if len(failed) > 0 {
		logger.Warn("旧备份清理失败（旧包保留，不影响本次备份）", zap.Strings("errors", failed))
	}
	return deleted, failed
}

// SchedulerStatus 调度器状态快照（诊断/管理面用；**不阻塞**）。
type SchedulerStatus struct {
	// Dir 备份目录。
	Dir string "json:\"dir\""
	// Keep 保留份数（`<=0` 表示不限）。
	Keep int "json:\"keep\""
	// At 每日时刻（HH:MM）。
	At string "json:\"at\""
	// Running 当前是否正在备份。
	Running bool "json:\"running\""
	// LastRunAt 本进程上一次发起备份的时刻（重启后清零，不代表从未备份过）。
	LastRunAt time.Time "json:\"last_run_at\""
	// LastFile / LastSize 上一次成功的包（本进程视角）。
	LastFile string "json:\"last_file\""
	LastSize int64  "json:\"last_size\""
	// LastError 上一次失败原因（成功一次即清空）。
	LastError string "json:\"last_error\""
}

// Status 返回状态快照。
func (s *Scheduler) Status() SchedulerStatus {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return SchedulerStatus{
		Dir:       s.dir,
		Keep:      s.keep,
		At:        s.at.String(),
		Running:   s.running,
		LastRunAt: s.lastRun,
		LastFile:  s.lastFile,
		LastSize:  s.lastSize,
		LastError: s.lastErr,
	}
}

func (s *Scheduler) setRunning(v bool) {
	s.stateMu.Lock()
	s.running = v
	s.stateMu.Unlock()
}

func (s *Scheduler) setError(msg string) {
	s.stateMu.Lock()
	s.lastErr = msg
	s.stateMu.Unlock()
}

func (s *Scheduler) recordFailure(started time.Time, err error) {
	s.stateMu.Lock()
	s.lastRun = started
	s.lastErr = err.Error()
	s.stateMu.Unlock()
	logger.Warn("auto backup failed", zap.Error(err))
}
