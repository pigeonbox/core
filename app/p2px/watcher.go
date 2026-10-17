// watcher.go — 信箱 watcher(2026-10-14 在线名录+邀请信箱):长驻
// `p2pc mailbox --watch` 子进程 + 宿主侧守护。
//
// 生命周期:与传输任务解耦——非 Task(不占并发闸、无 specSnapshot 重派,
// 既有 task monitor 只认任务表);守护在 monitorLoop 每 tick 对账:
// 期望态 = 直传 enabled && discoverable(opt-in) && standalone && 二进制
// 在位;进程死 = 指数退避重启(1s 起、60s 封顶,tick 粒度),Stop = 杀进程。
// stdout = NDJSON 邀请事件流({"event":"invite","code":...,"name":...,
// "note":...}) → AddInvite(P3 收件箱,UI/API 零改动复用);开箱验真在
// p2pc 侧以名录 box_pub 完成,宿主只收验真后的口令。
package p2px

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/pigeonbox/core/pkg/logger"
	"io"

	"github.com/oekit/concurrency/async"
	"github.com/pigeonbox/core/conf"
	"os/exec"
	"syscall"
	"time"
)

const (
	// watcherIntervalArgs p2pc 轮询间隔(与 monitorLoop tick 同量级)。
	watcherIntervalArgs = "5s"
	// watcherBackoffBase / watcherBackoffMax 进程连续失败退避(tick 粒度生效)。
	watcherBackoffBase = 1 * time.Second
	watcherBackoffMax  = 60 * time.Second
)

// watcherState 守护状态(并发安全经 watchMu)。
type watcherState struct {
	cmd      *exec.Cmd
	pid      int
	fails    int       // 连续异常退出次数(退避指数)
	retryAt  time.Time // 退避到期时刻(零值=立即可重启)
	watching bool      // 期望态缓存(日志防抖)
}

// reconcileWatcher 期望态对账(monitorLoop 每 tick 调用)。
func (s *Service) reconcileWatcher() {
	cfg, bin, ok := s.watcherDesired()
	if !ok {
		s.stopWatcher("期望态不满足")
		return
	}
	s.watchMu.Lock()
	run := s.watch.cmd != nil
	wait := s.watch.retryAt
	s.watchMu.Unlock()
	if run {
		return
	}
	if !wait.IsZero() && s.now().Before(wait) {
		return // 退避中
	}
	if err := s.spawnWatcher(cfg, bin); err != nil {
		s.bumpWatcherBackoff(err)
	}
}

// watcherDesired 期望态判定:standalone + 直传 enabled + discoverable + 二进制在位。
func (s *Service) watcherDesired() (conf.P2PDirectConfig, string, bool) {
	if s.deps.Standalone != nil && !s.deps.Standalone() {
		return conf.P2PDirectConfig{}, "", false
	}
	cfg, ok := s.deps.GetDirect(context.Background())
	if !ok || !cfg.Enabled || !cfg.Discoverable {
		return conf.P2PDirectConfig{}, "", false
	}
	bin, err := s.locateBinary(cfg)
	if err != nil {
		return conf.P2PDirectConfig{}, "", false
	}
	return cfg, bin, true
}

// spawnWatcher 拉起长驻信箱 watcher。
func (s *Service) spawnWatcher(cfg conf.P2PDirectConfig, bin string) error {
	args := []string{
		"mailbox", "--watch", "--interval", watcherIntervalArgs, "--discoverable",
		"--registry", effectiveRegistry(cfg.RegistryURL),
		"--key", s.nodeKeyPath(),
	}
	if name := s.deps.NodeName; name != "" {
		args = append(args, "--name", name)
	}
	cmd := exec.Command(bin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("p2pc mailbox 启动失败: %w", err)
	}
	s.watchMu.Lock()
	s.watch.cmd = cmd
	s.watch.pid = cmd.Process.Pid
	s.watch.watching = true
	s.watchMu.Unlock()
	logger.Info(fmt.Sprintf("[p2px] 信箱 watcher 已启动(pid=%d registry=%s)", cmd.Process.Pid, effectiveRegistry(cfg.RegistryURL)))
	// watcher 三协程 GoSafeLabeled:panic 走全局 zap handler 带任务标签,不静默打死进程
	async.GoSafeLabeled("p2pc-watcher-scan", func() { s.scanInvites(stdout) })
	async.GoSafeLabeled("p2pc-watcher-stderr", func() { s.passthroughWatcherStderr(stderr) })
	async.GoSafeLabeled("p2pc-watcher-wait", func() { s.waitWatcher(cmd) })
	return nil
}

// scanInvites 邀请事件解析(NDJSON 单行;非 invite 事件忽略)。
func (s *Service) scanInvites(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		var ev struct {
			Event string `json:"event"`
			Code  string `json:"code"`
			From  string `json:"from"`
			Name  string `json:"name"`
			Note  string `json:"note"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Event != "invite" {
			continue
		}
		if _, err := s.AddInvite(ev.Code, ev.Name, ev.Note); err != nil {
			logger.Warn(fmt.Sprintf("[p2px] 名录邀请入箱失败(from=%s): %v", ev.From, err))
			continue
		}
		logger.Info(fmt.Sprintf("[p2px] 收到名录邀请 from=%s code=%s…", ev.From, shortCode(ev.Code)))
	}
}

// passthroughWatcherStderr watcher 诊断输出直达服务日志(排障可见)。
func (s *Service) passthroughWatcherStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		logger.Info(fmt.Sprintf("[p2px][watcher] %s", sc.Text()))
	}
}

// waitWatcher 进程退出记账(退避指数增长;期望态关闭不记失败)。
func (s *Service) waitWatcher(cmd *exec.Cmd) {
	err := cmd.Wait()
	s.watchMu.Lock()
	s.watch.cmd = nil
	s.watch.pid = 0
	s.watch.fails++
	fails := s.watch.fails
	s.watchMu.Unlock()
	if s.stopped() {
		return // Stop 路径的退出是预期,不算失败
	}
	reason := "退出"
	if err != nil {
		reason = "退出: " + err.Error()
	}
	s.bumpWatcherBackoff(fmt.Errorf("watcher %s(连续第 %d 次)", reason, fails))
}

// bumpWatcherBackoff 失败后退避推进(1s、2s、4s…60s 封顶)。
func (s *Service) bumpWatcherBackoff(err error) {
	s.watchMu.Lock()
	delay := watcherBackoffBase << uint(min(s.watch.fails, 6))
	if delay > watcherBackoffMax {
		delay = watcherBackoffMax
	}
	s.watch.retryAt = s.now().Add(delay)
	s.watchMu.Unlock()
	logger.Warn(fmt.Sprintf("[p2px] %v,%v 后重试", err, delay))
}

// stopWatcher 停 watcher(期望态不满足/Stop;幂等)。
func (s *Service) stopWatcher(reason string) {
	s.watchMu.Lock()
	cmd := s.watch.cmd
	pid := s.watch.pid
	had := cmd != nil
	s.watch.cmd = nil
	s.watch.pid = 0
	s.watch.fails = 0
	s.watch.retryAt = time.Time{}
	s.watchMu.Unlock()
	if had {
		killProcessGroup(pid)
		logger.Info(fmt.Sprintf("[p2px] 信箱 watcher 已停止(%s)", reason))
	}
}

// stopped Stop 是否已触发(Stop 后退出不计失败)。
func (s *Service) stopped() bool {
	select {
	case <-s.monStop:
		return true
	default:
		return false
	}
}

// shortCode 口令日志截断(全量口令不进日志,与任务日志同基调)。
func shortCode(code string) string {
	if len(code) <= 4 {
		return code
	}
	return code[:4] + "…"
}
