// Package p2px P2P 设备直传 supervisor（2026-10-14 对称节点 spec §7）。
//
// 职责切分：数据面走子进程（每任务 spawn p2pc，进程组 kill 即取消，与
// desktop sidecar 语义一致——p2pc 协议层无 ctx 取消，kill 是唯一取消面）；
// 控制面进程内：任务编排/口令与输出解析/配额监控/孤儿清扫。standalone 专属
// （多副本语义未定义，与 federation 同约束）。
//
// 配置来源：SystemConfig p2p.direct 子段（DB 热生效），经 Deps.GetDirect 注入
// （构造注入，棘轮规则 8：不直读全局 conf）。env/yaml 不参与直传段——后台即真相。
package p2px

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pigeonbox/core/conf"
)

// 任务状态机。
const (
	StateWaiting  = "waiting"  // 已 spawn，等待对端接入/传输启动
	StateRunning  = "running"  // 传输进行中（收到口令行之外的首个输出）
	StateDone     = "done"
	StateFailed   = "failed"
	StateCanceled = "canceled"
	StateRetrying = "retrying" // 失败退避中，稍后自动重派（断点续传环）
)

const (
	// defaultMaxTaskBytes 单任务字节上限缺省 50 GiB（spec §5）
	defaultMaxTaskBytes = 50 << 30
	// defaultInboxQuotaBytes 收件区总配额缺省 200 GiB
	defaultInboxQuotaBytes = 200 << 30
	// defaultPublicRegistry 官方公共注册中心（与 federation 缺省同源，spec §5）
	defaultPublicRegistry = "http://home.enjoye.top:56788"
	// logRingSize 任务日志环形缓冲行数
	logRingSize = 256
	// defaultMaxConcurrent 在途任务上限（实测评审：无闸会拖垮 NAS 磁盘；
	// spec §7.2 原设。超出响亮拒绝，取消或等终态后再发起）
	defaultMaxConcurrent = 2
	// 自动续传环（2026-10-14 GB 级设计）：网络类失败退避后自动重派同口令
	// 任务，发端重发公告、收端重跑同目录——p2pc 协议层按 offset 断点续传。
	// 83.5s 定长墙（transport 已知限制）由此降级为分段成本。
	// 预算可经 Deps 覆盖（E2E/特殊部署；缺省 8 次、指数 5s 封顶 60s——
	// 收端额外 +3s 错峰等发端先重发公告；总重试窗口 ≈5 分钟）
	maxAttempts  = 8
	retryBackoff = 5 * time.Second
	// recvGhostPairTimeout 收端幽灵配对看门狗（2026-10-14 冻结实测）：与死会话
	// 配对后 p2pc 清空读 deadline 无限等数据（p2pd 回放死会话 hello）——
	// 真实配对在双端就绪后秒级开始出字节，90s 零字节必是幽灵，杀+重派
	recvGhostPairTimeout = 90 * time.Second
)

// Deps supervisor 依赖注入（生产由 bootstrap/wiring 装配；测试注入桩）。
type Deps struct {
	// Standalone 部署形态判定（棘轮规则 8：构造注入，不直读全局 conf）
	Standalone func() bool
	// DataDir 服务数据目录（默认收件区/任务工作区基准）
	DataDir string
	// GetDirect 读当前直传配置（DB 热生效；bool=false 表示未配置）
	GetDirect func(ctx context.Context) (conf.P2PDirectConfig, bool)
	// MonitorInterval 配额监控周期（缺省 5s；测试可压到毫秒级）
	MonitorInterval time.Duration
	// MaxAttempts 自动续传最大重试次数（缺省 8；0=缺省）
	MaxAttempts int
	// GhostPairTimeout 收端零字节配对看门狗（缺省 90s；测试可调）
	GhostPairTimeout time.Duration
	// InboxTTL 收件区文件存活期（缺省 7 天；终态/已发布/无属主目录超期即清扫，spec §7.3）
	InboxTTL time.Duration
	// NodeName 名录显示名（站点名；信箱 watcher --name 注入，缺省 p2pc）
	NodeName string
}

// Task 直传任务（内存态；重启即 expired 语义——pid 文件清扫兜底）。
type Task struct {
	ID        string
	Kind      string // send | recv
	Code      string
	Paths     []string
	BytesDone int64
	CreatedAt time.Time

	mu         sync.Mutex
	state      string
	err        string
	finishedAt time.Time
	published  bool // 已发布为分享（幂等闸：重复/并发 publish 拒绝——否则重复铸分享+重复拷存储）
	attempts   int        // 已尝试次数（自动续传环）
	waitingSince time.Time // 进入 waiting 的时刻（收端幽灵配对看门狗用）
	spec       *spawnSpec // 重派规格（spawn 时记录）
	retryTimer *time.Timer
	log        []string
	pid        int
	outDir     string // recv 任务的落盘目录
	cancel     context.CancelFunc
}

// TaskSnapshot 对外只读快照（handler 消费）。
type TaskSnapshot struct {
	ID         string
	Kind       string
	State      string
	Code       string
	Paths      []string
	BytesDone  int64
	Error      string
	Attempts   int
	CreatedAt  time.Time
	FinishedAt time.Time
}

// Snapshot 并发安全快照。
func (t *Task) Snapshot() TaskSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return TaskSnapshot{
		ID: t.ID, Kind: t.Kind, State: t.state, Code: t.Code,
		Paths: append([]string(nil), t.Paths...), BytesDone: t.BytesDone,
		Error: t.err, Attempts: t.attempts,
		CreatedAt: t.CreatedAt, FinishedAt: t.finishedAt,
	}
}

// Status admin 状态面。
type Status struct {
	Available     bool
	BinaryPath    string
	Enabled       bool
	RegistryURL   string
	RecvDir       string
	InboxUsed     int64
	InboxQuota    int64
	DeployAllowed bool
}

// Service supervisor 服务。
type Service struct {
	deps Deps

	mu    sync.Mutex
	tasks map[string]*Task

	invMu   sync.Mutex
	invites []Invite // P3 收件箱（内存态，TTL 10min/cap 20）

	// 信箱 watcher（2026-10-14 在线名录+邀请信箱；watcher.go）
	watchMu sync.Mutex
	watch   watcherState

	nowFn   func() time.Time // 测试注入；缺省 time.Now
	monStop chan struct{}
	monWg   sync.WaitGroup
}

// now 当前时间（可注入）。
func (s *Service) now() time.Time {
	if s.nowFn != nil {
		return s.nowFn()
	}
	return time.Now()
}

// NewService 构造（幂等清扫孤儿 + 起配额监控协程）。
func NewService(deps Deps) *Service {
	if deps.MonitorInterval <= 0 {
		deps.MonitorInterval = 5 * time.Second
	}
	if deps.MaxAttempts <= 0 {
		deps.MaxAttempts = maxAttempts
	}
	if deps.GhostPairTimeout <= 0 {
		deps.GhostPairTimeout = recvGhostPairTimeout
	}
	s := &Service{
		deps:    deps,
		tasks:   map[string]*Task{},
		monStop: make(chan struct{}),
	}
	s.sweepOrphans()
	s.monWg.Add(1)
	go s.monitorLoop()
	return s
}

// Stop 停机（Cleanup 调用；幂等）：杀全部在途子进程 + 停监控。
func (s *Service) Stop() error {
	select {
	case <-s.monStop:
	default:
		close(s.monStop)
	}
	s.mu.Lock()
	tasks := make([]*Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		tasks = append(tasks, t)
	}
	s.mu.Unlock()
	for _, t := range tasks {
		t.mu.Lock()
		if t.state == StateWaiting || t.state == StateRunning {
			killProcessGroup(t.pid)
			t.state = StateCanceled
			t.err = "server shutdown"
			t.finishedAt = time.Now()
			s.removePidFile(t.ID)
		}
		t.mu.Unlock()
	}
	s.stopWatcher("server shutdown") // 信箱 watcher 随 supervisor 收口
	s.monWg.Wait()
	return nil
}

// List 任务列表（创建时间倒序）。
func (s *Service) List(ctx context.Context) []*TaskSnapshot {
	s.mu.Lock()
	tasks := make([]*Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		tasks = append(tasks, t)
	}
	s.mu.Unlock()
	out := make([]*TaskSnapshot, 0, len(tasks))
	for _, t := range tasks {
		snap := t.Snapshot()
		out = append(out, &snap)
	}
	return out
}

// Cancel 取消任务（kill 进程组）。
func (s *Service) Cancel(ctx context.Context, taskID string) error {
	t, err := s.getTask(taskID)
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state != StateWaiting && t.state != StateRunning {
		return fmt.Errorf("任务 %s 已终态（%s），不可取消", taskID, t.state)
	}
	if t.cancel != nil {
		t.cancel()
	}
	t.stopRetryLocked()
	killProcessGroup(t.pid)
	t.state = StateCanceled
	t.finishedAt = time.Now()
	s.removePidFile(t.ID)
	return nil
}

// Status 状态面：二进制探测 + 生效配置 + 收件区用量。
func (s *Service) Status(ctx context.Context) (Status, error) {
	cfg, ok := s.deps.GetDirect(ctx)
	st := Status{DeployAllowed: s.deps.Standalone == nil || s.deps.Standalone()}
	if ok {
		st.Enabled = cfg.Enabled
		st.RegistryURL = effectiveRegistry(cfg.RegistryURL)
		st.RecvDir = s.effectiveRecvDir(cfg)
		st.InboxQuota = effectiveQuota(cfg.InboxQuotaBytes, defaultInboxQuotaBytes)
		if st.RecvDir != "" {
			st.InboxUsed = dirSize(st.RecvDir)
		}
	}
	if bin, err := s.locateBinary(cfg); err == nil {
		st.Available = true
		st.BinaryPath = bin
	}
	return st, nil
}

// getTask 取任务（不存在报错）。
func (s *Service) getTask(taskID string) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[taskID]
	if !ok {
		return nil, fmt.Errorf("任务不存在: %s", taskID)
	}
	return t, nil
}

// guard 创建任务前的公共门禁：standalone + 开关 + 二进制。
func (s *Service) guard(ctx context.Context) (conf.P2PDirectConfig, string, error) {
	if s.deps.Standalone != nil && !s.deps.Standalone() {
		return conf.P2PDirectConfig{}, "", errors.New("设备直传为 standalone 部署专属（多副本模式禁用）")
	}
	cfg, ok := s.deps.GetDirect(ctx)
	if !ok || !cfg.Enabled {
		return conf.P2PDirectConfig{}, "", errors.New("设备直传未启用（管理后台「P2P 与联邦」分区开启）")
	}
	bin, err := s.locateBinary(cfg)
	if err != nil {
		return conf.P2PDirectConfig{}, "", err
	}
	if n := s.activeCount(); n >= defaultMaxConcurrent {
		return conf.P2PDirectConfig{}, "", fmt.Errorf("在途任务已达上限（%d/%d），请等待完成或取消后再发起", n, defaultMaxConcurrent)
	}
	return cfg, bin, nil
}

// MaxAttemptsFor 生效重试预算（Deps 覆盖 > 缺省）。
func (d Deps) MaxAttemptsFor(_ *Task) int {
	if d.MaxAttempts > 0 {
		return d.MaxAttempts
	}
	return maxAttempts
}

// activeCount 在途（waiting/running）任务数。
func (s *Service) activeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, t := range s.tasks {
		t.mu.Lock()
		if t.state == StateWaiting || t.state == StateRunning || t.state == StateRetrying {
			n++
		}
		t.mu.Unlock()
	}
	return n
}

// effectiveRegistry 注册中心缺省回填（spec §5：留空=官方公共）。
func effectiveRegistry(u string) string {
	if strings.TrimSpace(u) == "" {
		return defaultPublicRegistry
	}
	return strings.TrimSpace(u)
}

// effectiveRecvDir 收件区缺省回填（留空=<data>/p2p-inbox）。
func (s *Service) effectiveRecvDir(cfg conf.P2PDirectConfig) string {
	if strings.TrimSpace(cfg.RecvDir) != "" {
		return strings.TrimSpace(cfg.RecvDir)
	}
	return filepath.Join(s.deps.DataDir, "p2p-inbox")
}

// effectiveQuota 配额缺省回填（0=缺省值）。
func effectiveQuota(v, def int64) int64 {
	if v > 0 {
		return v
	}
	return def
}

// validateSendPaths 发送路径白名单（仿 local_import 三层：绝对化/禁越界/EvalSymlinks 归一）。
func validateSendPaths(paths []string, roots []string) error {
	if len(paths) == 0 {
		return errors.New("paths 不能为空")
	}
	if len(roots) == 0 {
		return errors.New("未配置 send_roots 白名单（管理后台「P2P 与联邦」分区），拒绝所有发送")
	}
	cleanRoots := make([]string, 0, len(roots))
	for _, r := range roots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		real, err := filepath.EvalSymlinks(r)
		if err != nil {
			real = filepath.Clean(r)
		}
		cleanRoots = append(cleanRoots, real)
	}
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return fmt.Errorf("路径不可用: %s", p)
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return fmt.Errorf("路径不可用（symlink 解析失败）: %s", p)
		}
		ok := false
		for _, root := range cleanRoots {
			if real == root || strings.HasPrefix(real, root+string(os.PathSeparator)) {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("路径不在 send_roots 白名单内: %s", p)
		}
	}
	return nil
}

// newTaskID 短随机任务 ID（16 hex）。
func newTaskID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// PublishPaths 收件任务的可发布绝对路径（三层防穿越：任务态校验+目录归属+symlink 归一）。
// 只列顶层常规文件（目录由 share 侧逐文件递归处理不在本期范围）。
// 幂等闸：已发布过的任务拒绝（重复/并发 publish 会重复铸分享+重复拷存储）。
func (s *Service) PublishPaths(ctx context.Context, taskID string) ([]string, error) {
	t, err := s.getTask(taskID)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	if t.published {
		t.mu.Unlock()
		return nil, errors.New("该任务已发布过分享（重复发布被阻止；如需再次分享请重新接收）")
	}
	outDir := t.outDir
	state := t.state
	kind := t.Kind
	t.mu.Unlock()
	if kind != "recv" || state != StateDone {
		return nil, fmt.Errorf("仅已完成（done）的接收任务可发布（当前 %s/%s）", kind, state)
	}
	if outDir == "" {
		return nil, errors.New("任务无落盘目录")
	}
	cfg, ok := s.deps.GetDirect(ctx)
	if !ok {
		return nil, errors.New("直传配置缺失")
	}
	recvRoot := s.effectiveRecvDir(cfg)
	realRoot, err := filepath.EvalSymlinks(recvRoot)
	if err != nil {
		return nil, fmt.Errorf("收件区不可用: %w", err)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		p := filepath.Join(outDir, e.Name())
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			continue
		}
		// 落盘文件必须仍在收件区内（symlink 逃逸防御）
		if real != realRoot && !strings.HasPrefix(real, realRoot+string(os.PathSeparator)) {
			continue
		}
		info, err := os.Stat(real)
		if err != nil || info.IsDir() {
			continue // 目录暂不整体发布（多文件逐文件发布）
		}
		out = append(out, real)
	}
	if len(out) == 0 {
		return nil, errors.New("收件目录无文件")
	}
	return out, nil
}

// stopRetryLocked 停重试调度（调用方须持 t.mu；幂等）。
func (t *Task) stopRetryLocked() {
	if t.retryTimer != nil {
		t.retryTimer.Stop()
		t.retryTimer = nil
	}
}

// specSnapshot 取重派规格（并发安全）。
func (t *Task) specSnapshot() *spawnSpec {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.spec == nil {
		return nil
	}
	cp := *t.spec
	cp.args = append([]string(nil), t.spec.args...)
	cp.env = append([]string(nil), t.spec.env...)
	return &cp
}

// MarkPublished 置发布标记（handler 在全部文件铸分享成功后调用）。
func (s *Service) MarkPublished(taskID string) {
	t, err := s.getTask(taskID)
	if err != nil {
		return
	}
	t.mu.Lock()
	t.published = true
	t.mu.Unlock()
}

// sweepInbox 收件区 TTL 清扫（spec §7.3 防线落地）：终态/已发布任务目录与
// 无属主目录（重启后任务表丢失）超 InboxTTL 即删除。只认 16-hex 任务目录名
// ——管理员手工放进收件区的文件绝不动。
func (s *Service) sweepInbox() {
	cfg, ok := s.deps.GetDirect(context.Background())
	if !ok {
		return
	}
	recvDir := s.effectiveRecvDir(cfg)
	ttl := s.deps.InboxTTL
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	entries, err := os.ReadDir(recvDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !isTaskDirName(e.Name()) {
			continue
		}
		dir := filepath.Join(recvDir, e.Name())
		// 在途任务目录绝不动
		if t, err := s.getTask(e.Name()); err == nil {
			t.mu.Lock()
			active := t.state == StateWaiting || t.state == StateRunning
			created := t.CreatedAt
			t.mu.Unlock()
			if active {
				continue
			}
			if time.Since(created) <= ttl {
				continue
			}
		} else {
			// 无属主目录（进程重启任务表丢失）：按目录 mtime 判龄
			info, err := e.Info()
			if err != nil || time.Since(info.ModTime()) <= ttl {
				continue
			}
		}
		_ = os.RemoveAll(dir)
	}
}

// isTaskDirName 任务目录名形态（16 hex）。
func isTaskDirName(name string) bool {
	if len(name) != 16 {
		return false
	}
	for _, c := range name {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') { // 非 hex 即非任务目录名
			return false
		}
	}
	return true
}

// pidDir 任务 pid 文件目录（孤儿清扫用）。
func (s *Service) pidDir() string {
	return filepath.Join(s.deps.DataDir, "p2p-tasks")
}

func (s *Service) writePidFile(id string, pid int) {
	dir := s.pidDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, id+".pid"), []byte(fmt.Sprint(pid)), 0o600)
}

func (s *Service) removePidFile(id string) {
	_ = os.Remove(filepath.Join(s.pidDir(), id+".pid"))
}

// sweepOrphans 启动清扫：杀遗留 p2pc 进程组并清 pid 文件
// （server 被 kill -9 时子进程可能存活——fnos panic 风暴教训）。
func (s *Service) sweepOrphans() {
	dir := s.pidDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".pid") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var pid int
		if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err != nil || pid <= 0 {
			continue
		}
		killProcessGroup(pid) // 进程组=任务子进程；进程已死则 ESRCH 忽略
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

// killProcessGroup 杀进程组（p2pc 单进程，进程组兜底未来子进程）。
func killProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

// dirSize 目录递归字节数（配额统计）。
func dirSize(root string) int64 {
	var total int64
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}
