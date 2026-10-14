// spawn.go — p2pc 子进程生命周期：spawn/输出解析/配额监控/进程组取消。
package p2px

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pigeonbox/core/conf"
)

// Send 发起发送任务（白名单校验 → spawn p2pc send → 口令行异步解析进任务）。
func (s *Service) Send(ctx context.Context, paths []string) (*Task, error) {
	cfg, bin, err := s.guard(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateSendPaths(paths, cfg.SendRoots); err != nil {
		return nil, err
	}
	args := []string{"send"}
	args = append(args, paths...)
	args = append(args, s.registryArgs(cfg)...)
	return s.spawnPrepared(s.newTask("send", "", paths), bin, args, "")
}

// Recv 发起接收任务（口令 → spawn p2pc recv --yes --out <收件区>/<taskID>）。
func (s *Service) Recv(ctx context.Context, code string) (*Task, error) {
	cfg, bin, err := s.guard(ctx)
	if err != nil {
		return nil, err
	}
	code = normalizeCode(code)
	if code == "" {
		return nil, fmt.Errorf("口令不能为空")
	}
	recvDir := s.effectiveRecvDir(cfg)
	// 收件区总配额预检（已有占用 + 任务上限不得超总配额）
	quota := effectiveQuota(cfg.InboxQuotaBytes, defaultInboxQuotaBytes)
	if used := dirSize(recvDir); used >= quota {
		return nil, fmt.Errorf("收件区已满（%d/%d 字节），清理后重试", used, quota)
	}
	t := s.newTask("recv", code, nil)
	outDir := filepath.Join(recvDir, t.ID)
	// 口令走 env(PB_P2P_CODE)而非 argv——argv 在 ps 中同机可见,口令即授权
	// (2026-10-14 安全评审硬化;p2pc ≥v0.6.2 支持 "recv -" 读 env)
	args := []string{"recv", "-", "--out", outDir, "--yes"}
	args = append(args, s.registryArgs(cfg)...)
	return s.spawnPrepared(t, bin, args, outDir, "PB_P2P_CODE="+code)
}

// normalizeCode 口令归一：去空格/连字符、大写（与 p2pc 接受形态对齐后重新分组）。
func normalizeCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	code = strings.ReplaceAll(code, "-", "")
	if code == "" {
		return ""
	}
	// 4-4-4 分组回连字符（p2pc 接受裸串，规整后展示/日志一致）
	if len(code) == 12 {
		return code[:4] + "-" + code[4:8] + "-" + code[8:]
	}
	return code
}

// registryArgs registry/relay/node-key 参数拼装。
func (s *Service) registryArgs(cfg conf.P2PDirectConfig) []string {
	args := []string{"--registry", effectiveRegistry(cfg.RegistryURL)}
	if strings.TrimSpace(cfg.RelayAddr) != "" {
		args = append(args, "--relay", strings.TrimSpace(cfg.RelayAddr))
	}
	args = append(args, "--key", s.nodeKeyPath())
	return args
}

// nodeKeyPath 节点身份密钥路径（独立于 federation 密钥：<data>/p2p/node.key）。
func (s *Service) nodeKeyPath() string {
	return filepath.Join(s.deps.DataDir, "p2p", "node.key")
}

// newTask 建任务并入册。
func (s *Service) newTask(kind, code string, paths []string) *Task {
	t := &Task{
		ID:        newTaskID(),
		Kind:      kind,
		Code:      code,
		Paths:     append([]string(nil), paths...),
		CreatedAt: time.Now(),
		state:     StateWaiting,
	}
	s.mu.Lock()
	s.tasks[t.ID] = t
	s.mu.Unlock()
	return t
}

// spawnPrepared 真实 spawn：进程组 + stdout/stderr 行扫描 + Wait 收尾。
// extraEnv 为任务级环境变量（recv 的 PB_P2P_CODE 口令走此通道,不落 argv）。
func (s *Service) spawnPrepared(t *Task, bin string, args []string, outDir string, extraEnv ...string) (*Task, error) {
	taskCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(taskCtx, bin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	if outDir != "" {
		if err := os.MkdirAll(outDir, 0o700); err != nil {
			cancel()
			s.dropTask(t.ID)
			return nil, fmt.Errorf("收件目录创建失败: %w", err)
		}
		t.mu.Lock()
		t.outDir = outDir
		t.mu.Unlock()
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		s.dropTask(t.ID)
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		s.dropTask(t.ID)
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		s.dropTask(t.ID)
		return nil, fmt.Errorf("p2pc 启动失败: %w", err)
	}
	t.mu.Lock()
	t.cancel = cancel
	t.pid = cmd.Process.Pid
	t.mu.Unlock()
	s.writePidFile(t.ID, cmd.Process.Pid)

	// 记录重派规格（自动续传环：失败退避后同参重spawn；send 重试补 --code 复用口令）
	retryArgs := args
	if t.Kind == "send" && t.Code != "" && !sliceHas(args, "--code") {
		retryArgs = append(append([]string(nil), args...), "--code", t.Code)
	}
	t.mu.Lock()
	t.spec = &spawnSpec{bin: bin, args: retryArgs, outDir: outDir, env: append([]string(nil), extraEnv...)}
	t.mu.Unlock()

	// stdout 行扫描：口令行（send）→ Code；首个业务行 → running；全部入环形日志
	go s.scanLines(t, stdout, true)
	go s.scanLines(t, stderr, false)
	go s.waitTask(t, cmd)
	return t, nil
}

// sliceHas 判断参数表是否已含某旗标（重派防重复追加）。
func sliceHas(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// spawnSpec 重派规格（自动续传环）。
type spawnSpec struct {
	bin    string
	args   []string
	outDir string
	env    []string
}

// isRetryable 网络类失败分类（自动续传环白名单；未知错误默认不重试——
// fail-safe：配额/白名单/格式类一律终态，绝不空转）。
func isRetryable(err string) bool {
	for _, kw := range []string{
		"timeout", "no recent network activity", "对端已离开", "已离开",
		"connection refused", "broken pipe", "EOF", "解析: 全部注册中心", "i/o timeout",
	} {
		if strings.Contains(err, kw) {
			return true
		}
	}
	return false
}

// fireRetry 退避到期重派（自动续传环：同 code 同参数；收端同目录触发
// p2pc 协议层 offset 断点续传）。
func (s *Service) fireRetry(t *Task) {
	t.mu.Lock()
	if t.state != StateRetrying {
		t.mu.Unlock()
		return // 已被取消/终态
	}
	t.mu.Unlock()
	cfg, ok := s.deps.GetDirect(context.Background())
	if !ok || !cfg.Enabled {
		s.finishTask(t, StateFailed, "重试时直传已禁用")
		return
	}
	spec := t.specSnapshot()
	if spec == nil {
		s.finishTask(t, StateFailed, "重试规格缺失")
		return
	}
	if _, err := s.spawnPrepared(t, spec.bin, spec.args, spec.outDir, spec.env...); err != nil {
		s.finishTask(t, StateFailed, "重派失败: "+err.Error())
		return
	}
	t.mu.Lock()
	t.state = StateWaiting
	t.mu.Unlock()
}

// finishTask 写终态（重派失败路径复用）。
func (s *Service) finishTask(t *Task, state, msg string) {
	t.mu.Lock()
	t.state = state
	t.err = msg
	t.finishedAt = time.Now()
	t.mu.Unlock()
	s.removePidFile(t.ID)
}

// scanLines 行扫描（stderr 原样透传——协议版本不一致等文案直达面板）。
func (s *Service) scanLines(t *Task, r interface{ Read([]byte) (int, error) }, isStdout bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		t.mu.Lock()
		if isStdout && t.Code == "" {
			if i := strings.Index(line, "口令:"); i >= 0 {
				t.Code = strings.TrimSpace(line[i+len("口令:"):])
			} else if i := strings.Index(line, "口令："); i >= 0 { // 全角冒号防御
				t.Code = strings.TrimSpace(line[i+len("口令："):])
			}
		}
		if t.state == StateWaiting && !strings.HasPrefix(line, "口令") {
			t.state = StateRunning
		}
		t.log = append(t.log, line)
		if len(t.log) > logRingSize {
			t.log = t.log[len(t.log)-logRingSize:]
		}
		t.mu.Unlock()
	}
}

// waitTask 进程退出定终态（退出码即真相）。
func (s *Service) waitTask(t *Task, cmd *exec.Cmd) {
	err := cmd.Wait()
	t.mu.Lock()
	defer t.mu.Unlock()
	s.removePidFile(t.ID)
	// 终态不覆写:取消/配额杀已写过 failed/canceled 及真因——进程退出码
	// (signal: killed)只是结果,真因在先写入方(2026-10-14 矩阵测试发现
	// 配额原因被退出码覆盖,运维只见 killed 不见配额)
	if t.state == StateCanceled || t.state == StateFailed {
		return
	}
	t.finishedAt = time.Now()
	if err == nil {
		t.state = StateDone
		if t.outDir != "" {
			t.BytesDone = dirSize(t.outDir)
		}
		return
	}
	t.err = fmt.Sprintf("p2pc 退出: %v", err)
	if len(t.log) > 0 {
		t.err = t.err + "；" + t.log[len(t.log)-1]
	}
	// 自动续传环（2026-10-14 GB 级设计）：网络类失败退避重派，p2pc 协议层
	// 按 offset 断点续传——83.5s 定长墙等瞬断由此降级为分段成本
	if t.attempts < s.deps.MaxAttemptsFor(t) && isRetryable(t.err) && (t.Kind != "send" || t.Code != "") {
		t.attempts++
		t.state = StateRetrying
		delay := retryBackoff << uint(t.attempts-1)
		if delay > 60*time.Second {
			delay = 60 * time.Second
		}
		if t.Kind == "recv" {
			delay += 3 * time.Second // 错峰：等发端先重发公告
		}
		s.removePidFile(t.ID)
		t.retryTimer = time.AfterFunc(delay, func() { s.fireRetry(t) })
		return
	}
	t.state = StateFailed
}

// monitorLoop 配额监控 + 收件区 TTL 清扫（节流：生产每小时一轮；测试用小间隔时
// 每 ~720 tick 一轮，保证小 TTL 用例可触发）。
func (s *Service) monitorLoop() {
	defer s.monWg.Done()
	tick := time.NewTicker(s.deps.MonitorInterval)
	defer tick.Stop()
	lastSweep := time.Now()
	for {
		select {
		case <-s.monStop:
			return
		case <-tick.C:
			s.checkQuotas()
			if time.Since(lastSweep) >= time.Hour || s.deps.MonitorInterval < 10*time.Second && time.Since(lastSweep) >= s.deps.MonitorInterval*720 {
				s.sweepInbox()
				lastSweep = time.Now()
			}
		}
	}
}

// checkQuotas 单任务超限 + 收件区总量超限。
func (s *Service) checkQuotas() {
	s.mu.Lock()
	tasks := make([]*Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		tasks = append(tasks, t)
	}
	s.mu.Unlock()

	cfg, ok := s.deps.GetDirect(context.Background())
	if !ok {
		return
	}
	maxTask := effectiveQuota(cfg.MaxTaskBytes, defaultMaxTaskBytes)
	inboxQuota := effectiveQuota(cfg.InboxQuotaBytes, defaultInboxQuotaBytes)
	recvDir := s.effectiveRecvDir(cfg)

	for _, t := range tasks {
		t.mu.Lock()
		active := t.state == StateWaiting || t.state == StateRunning
		outDir := t.outDir
		t.mu.Unlock()
		if !active || outDir == "" {
			continue
		}
		size := dirSize(outDir)
		t.mu.Lock()
		t.BytesDone = size
		if size > maxTask {
			killProcessGroup(t.pid)
			t.state = StateFailed
			t.err = fmt.Sprintf("超过单任务配额（%d > %d 字节）", size, maxTask)
			t.finishedAt = time.Now()
			s.removePidFile(t.ID)
		}
		t.mu.Unlock()
	}
	// 收件区总量=整目录（含历史收件）：超配额时在途任务全部补杀
	// （新任务闸在 Recv 创建期；这里是多任务并发的最后一道）。
	if dirSize(recvDir) > inboxQuota {
		for _, t := range tasks {
			t.mu.Lock()
			if (t.state == StateWaiting || t.state == StateRunning) && t.outDir != "" {
				killProcessGroup(t.pid)
				t.state = StateFailed
				t.err = fmt.Sprintf("收件区总量超配额（%d 字节）", inboxQuota)
				t.finishedAt = time.Now()
				s.removePidFile(t.ID)
			}
			t.mu.Unlock()
		}
	}
}

// dropTask 创建失败时出册。
func (s *Service) dropTask(id string) {
	s.mu.Lock()
	delete(s.tasks, id)
	s.mu.Unlock()
}

// locateBinary 查找序：config.BinaryPath > PB_P2PC_PATH env > 内置候选
// （可执行文件同目录/p2pc、./p2pc、/usr/bin/p2pc、PATH 查找）。
func (s *Service) locateBinary(cfg conf.P2PDirectConfig) (string, error) {
	if p := strings.TrimSpace(cfg.BinaryPath); p != "" {
		if isExecutable(p) {
			return p, nil
		}
		return "", fmt.Errorf("配置的 p2pc 路径不可执行: %s", p)
	}
	if p := strings.TrimSpace(os.Getenv("PB_P2PC_PATH")); p != "" && isExecutable(p) {
		return p, nil
	}
	candidates := []string{}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "p2pc"))
	}
	candidates = append(candidates, "./p2pc", "/usr/bin/p2pc")
	for _, c := range candidates {
		if isExecutable(c) {
			abs, _ := filepath.Abs(c)
			return abs, nil
		}
	}
	if p, err := exec.LookPath("p2pc"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("未找到 p2pc 二进制（随包应在 /app/p2pc；或配置 p2p.direct.binary_path / PB_P2PC_PATH）")
}

// isExecutable 存在且可执行。
func isExecutable(p string) bool {
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&0o111 != 0
}
