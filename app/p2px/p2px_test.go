package p2px

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pigeonbox/core/app/federation"
	"github.com/pigeonbox/core/conf"
)

// withFederation 注入联邦信封实例（联邦域实例由装配层构造后注入；直传包不
// 构造——见 SetInviteMailbox/SetPeerDirectory）。测试显式注入以覆盖真实装配形态。
func withFederation(s *Service) *Service {
	s.SetInviteMailbox(federation.NewInviteMailbox(nil))
	s.SetPeerDirectory(federation.NewPeerDirectory(func(context.Context) (string, bool) {
		return "", false
	}))
	return s
}

// stub p2pc:send 打口令行后阻塞;recv 把 hello 落到 --out 目录后退出。
// quota-stub: recv 写一个超限文件。
func writeStub(t *testing.T, mode string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "p2pc")
	var body string
	switch mode {
	case "ok":
		body = `#!/bin/sh
case "$1" in
  send)
    echo "口令: ABCD-1234-WXYZ"
    echo "等待对端接入..."
    sleep 60
    ;;
  recv)
    out=""
    while [ $# -gt 0 ]; do
      if [ "$1" = "--out" ]; then out="$2"; shift 2; continue; fi
      shift
    done
    mkdir -p "$out"
    echo hello > "$out/hello.txt"
    echo "✓ 接收完成"
    ;;
  -v) echo "p2pc 0.0.0-stub" ;;
esac
`
	case "quota":
		body = `#!/bin/sh
case "$1" in
  recv)
    out=""
    while [ $# -gt 0 ]; do
      if [ "$1" = "--out" ]; then out="$2"; shift 2; continue; fi
      shift
    done
    mkdir -p "$out"
    dd if=/dev/zero of="$out/big.bin" bs=1024 count=2048 2>/dev/null
    sleep 30
    ;;
esac
`
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testDeps(t *testing.T, binPath string) Deps {
	t.Helper()
	rootsDir := t.TempDir() // 夹具目录只建一次——闭包内每次调用 t.TempDir()
	inboxDir := t.TempDir() // 会产出新路径,收件区/白名单地址必须稳定
	return Deps{
		Standalone:      func() bool { return true },
		DataDir:         t.TempDir(),
		MonitorInterval: 50 * time.Millisecond,
		GetDirect: func(ctx context.Context) (conf.P2PDirectConfig, bool) {
			return conf.P2PDirectConfig{
				Enabled:         true,
				RegistryURL:     "http://127.0.0.1:1",
				SendRoots:       []string{rootsDir},
				RecvDir:         inboxDir,
				MaxTaskBytes:    1024 * 1024, // 1MB
				InboxQuotaBytes: 10 * 1024 * 1024,
				BinaryPath:      binPath,
			}, true
		},
	}
}

// TestSendParseCodeAndCancel send 任务的口令解析与取消(kill 进程组)。
func TestSendParseCodeAndCancel(t *testing.T) {
	bin := writeStub(t, "ok")
	svc := NewService(testDeps(t, bin))
	defer func() { _ = svc.Stop() }()

	// 发送源必须在白名单内且真实存在（supervisor 的 symlink 归一要求存在性）
	cfg, _ := svc.deps.GetDirect(context.Background())
	src := filepath.Join(cfg.SendRoots[0], "hello.txt")
	if err := os.WriteFile(src, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	task, err := svc.Send(context.Background(), []string{src})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	// 等口令行被解析
	deadline := time.Now().Add(3 * time.Second)
	for task.Snapshot().Code == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := task.Snapshot().Code; got != "ABCD-1234-WXYZ" {
		t.Fatalf("口令解析失败: %q", got)
	}
	if err := svc.Cancel(context.Background(), task.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for task.Snapshot().State != StateCanceled && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if st := task.Snapshot().State; st != StateCanceled {
		t.Fatalf("取消后状态应为 canceled: %s", st)
	}
}

// TestRecvQuotaKill 收件超限被配额监控杀掉。
func TestRecvQuotaKill(t *testing.T) {
	bin := writeStub(t, "quota")
	svc := NewService(testDeps(t, bin))
	defer func() { _ = svc.Stop() }()

	task, err := svc.Recv(context.Background(), "ABCD-1234-WXYZ")
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := task.Snapshot().State
		if st == StateFailed || st == StateDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("配额监控未生效,状态停在 %s", st)
		}
		time.Sleep(30 * time.Millisecond)
	}
	if st := task.Snapshot(); st.State != StateFailed {
		t.Fatalf("超限应 failed: %+v", st)
	}
}

// TestStandaloneGate 非 standalone 响亮拒绝。
func TestStandaloneGate(t *testing.T) {
	bin := writeStub(t, "ok")
	d := testDeps(t, bin)
	d.Standalone = func() bool { return false }
	svc := NewService(d)
	defer func() { _ = svc.Stop() }()
	if _, err := svc.Send(context.Background(), []string{"/tmp/x"}); err == nil {
		t.Fatal("非 standalone 应拒绝")
	}
}

// TestSendRootsWhitelist 白名单外路径拒绝。
func TestSendRootsWhitelist(t *testing.T) {
	bin := writeStub(t, "ok")
	d := testDeps(t, bin)
	roots := []string{"/tmp/p2px-roots"}
	cfgFn := d.GetDirect
	d.GetDirect = func(ctx context.Context) (conf.P2PDirectConfig, bool) {
		c, ok := cfgFn(ctx)
		c.SendRoots = roots
		return c, ok
	}
	svc := NewService(d)
	defer func() { _ = svc.Stop() }()
	if _, err := svc.Send(context.Background(), []string{"/etc/passwd"}); err == nil {
		t.Fatal("白名单外路径应拒绝")
	}
}

// TestRecvDoneState 正常接收落盘即 done。
func TestRecvDoneState(t *testing.T) {
	bin := writeStub(t, "ok")
	svc := NewService(testDeps(t, bin))
	defer func() { _ = svc.Stop() }()
	task, err := svc.Recv(context.Background(), "ABCD-1234-WXYZ")
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for task.Snapshot().State != StateDone && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	st := task.Snapshot()
	if st.State != StateDone {
		t.Fatalf("应 done: %+v", st)
	}
	paths, err := svc.PublishPaths(context.Background(), task.ID)
	if err != nil || len(paths) != 1 {
		t.Fatalf("PublishPaths: %v %v", paths, err)
	}
	data, _ := os.ReadFile(paths[0])
	if string(data) != "hello\n" {
		t.Fatalf("落盘内容错: %q", data)
	}
}

// TestPublishIdempotency 重复发布被拒（幂等闸）。
func TestPublishIdempotency(t *testing.T) {
	bin := writeStub(t, "ok")
	svc := NewService(testDeps(t, bin))
	defer func() { _ = svc.Stop() }()
	task, err := svc.Recv(context.Background(), "ABCD-1234-WXYZ")
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for task.Snapshot().State != StateDone && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := svc.PublishPaths(context.Background(), task.ID); err != nil {
		t.Fatalf("首次发布取路径应成功: %v", err)
	}
	svc.MarkPublished(task.ID)
	if _, err := svc.PublishPaths(context.Background(), task.ID); err == nil {
		t.Fatal("重复发布应被拒")
	}
}

// TestInboxTTLSweep 超 TTL 目录被清扫；新目录与非任务形态目录不动（白盒直调）。
func TestInboxTTLSweep(t *testing.T) {
	bin := writeStub(t, "ok")
	d := testDeps(t, bin)
	d.InboxTTL = 80 * time.Millisecond
	svc := NewService(d)
	defer func() { _ = svc.Stop() }()

	cfg, _ := d.GetDirect(context.Background())
	old := filepath.Join(cfg.RecvDir, "0123456789abcdef")
	fresh := filepath.Join(cfg.RecvDir, "abcdef0123456789")
	custom := filepath.Join(cfg.RecvDir, "admin-stuff") // 非任务形态,绝不动
	for _, dir := range []string{old, fresh, custom} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "x.bin"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-time.Hour)
	for _, dir := range []string{old, custom} {
		if err := os.Chtimes(dir, past, past); err != nil {
			t.Fatal(err)
		}
	}
	svc.sweepInbox()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("超期任务目录未被清扫")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("新目录被误清扫")
	}
	if _, err := os.Stat(custom); err != nil {
		t.Fatal("非任务形态目录被误清扫")
	}
}

// ---- P3 邀请收发件箱（联邦信封管理；2026-10-22 实现搬至 app/federation）----

// TestInviteDelegation 联邦信封委托面:增/列/取三步落到联邦收件箱。
//
// 实现搬走后本包只做委托（生成物与端点行为不变）；收件箱的完整语义断言
// （去重/满箱/过期）随实现一并迁至 app/federation 的 TestMailboxInbox。
func TestInviteDelegation(t *testing.T) {
	bin := writeStub(t, "ok")
	svc := withFederation(NewService(testDeps(t, bin)))
	defer func() { _ = svc.Stop() }()

	// 格式校验（委托后仍拒）
	if _, err := svc.AddInvite("bad-code", "a", ""); err == nil {
		t.Fatal("坏口令应拒")
	}
	// 入箱 + 归一化(小写/无连字符输入)
	inv, err := svc.AddInvite("KJ8YWCYZE249", "发送站A", "note\n\x01x")
	if err != nil {
		t.Fatalf("入箱失败: %v", err)
	}
	if inv.Code != "KJ8Y-WCYZ-E249" {
		t.Fatalf("口令归一化错: %q", inv.Code)
	}
	if strings.ContainsAny(inv.Note, "\n\x01") {
		t.Fatalf("note 未消毒: %q", inv.Note)
	}
	// 委托目标即联邦收件箱
	if got := svc.inviteMailbox().ListInvites(); len(got) != 1 || got[0].ID != inv.ID {
		t.Fatalf("未委托到联邦收件箱: %+v", got)
	}
	if n := len(svc.ListInvites()); n != 1 {
		t.Fatalf("去重失败: %d", n)
	}
	// 认领出箱
	got, err := svc.TakeInvite(inv.ID)
	if err != nil || got.Code != "KJ8Y-WCYZ-E249" {
		t.Fatalf("认领失败: %+v %v", got, err)
	}
	if _, err := svc.TakeInvite(inv.ID); err == nil {
		t.Fatal("重复认领应拒")
	}
}

// TestQuotaReasonPreserved 配额杀的真因不被进程退出码覆写(矩阵测试发现)。
func TestQuotaReasonPreserved(t *testing.T) {
	bin := writeStub(t, "quota")
	d := testDeps(t, bin)
	svc := NewService(d)
	defer func() { _ = svc.Stop() }()
	task, err := svc.Recv(context.Background(), "ABCD-1234-WXYZ")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := task.Snapshot()
		if st.State == StateFailed {
			if !strings.Contains(st.Error, "配额") {
				t.Fatalf("配额真因被覆写: %q", st.Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("未达 failed: %+v", task.Snapshot())
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// TestMaxConcurrentCap 在途任务上限（实测评审：无闸会拖垮 NAS 磁盘）。
func TestMaxConcurrentCap(t *testing.T) {
	bin := writeStub(t, "ok")
	svc := NewService(testDeps(t, bin))
	defer func() { _ = svc.Stop() }()

	// 白名单内真实文件（supervisor 校验存在性）
	cfg, _ := svc.deps.GetDirect(context.Background())
	src := filepath.Join(cfg.SendRoots[0], "cap.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := svc.Send(context.Background(), []string{src}); err != nil {
			t.Fatalf("第%d个应成功: %v", i+1, err)
		}
	}
	if _, err := svc.Send(context.Background(), []string{src}); err == nil {
		t.Fatal("第3个应被上限拒绝")
	} else if !strings.Contains(err.Error(), "上限") {
		t.Fatalf("错误文案应含上限: %v", err)
	}
}

// ---- 自动续传环（GB 级设计：网络类失败退避重派，断点续传） ----

// flakyStub 第一次调用写半文件+网络类错误退出;第二次续写余下并成功。
// 计数落状态文件以跨进程存活（supervisor 每次 spawn 新进程）。
func writeFlakyStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "p2pc")
	body := `#!/bin/sh
CNT_FILE="$(dirname "$0")/.invocations"
N=$(( $(cat "$CNT_FILE" 2>/dev/null || echo 0) + 1 ))
echo "$N" > "$CNT_FILE"
OUT=""
while [ $# -gt 0 ]; do
  if [ "$1" = "--out" ]; then OUT="$2"; shift 2; continue; fi
  shift
done
mkdir -p "$OUT"
if [ "$N" = "1" ]; then
  head -c 512 /dev/zero > "$OUT/resume.bin"
  echo "✗ chunk 接收: timeout: no recent network activity" >&2
  exit 1
fi
head -c 1024 /dev/zero > "$OUT/resume.bin"
exit 0
`
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAutoResumeLoop 网络类失败自动重派（断点续传环）:重派→done、次数正确;
// 配额类失败绝不重试由 TestQuotaReasonPreserved 覆盖。
func TestAutoResumeLoop(t *testing.T) {
	bin := writeFlakyStub(t)
	svc := NewService(testDeps(t, bin))
	defer func() { _ = svc.Stop() }()
	task, err := svc.Recv(context.Background(), "ABCD-1234-WXYZ")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		st := task.Snapshot()
		if st.State == StateDone {
			if st.Attempts != 1 {
				t.Fatalf("重试次数应=1: %d", st.Attempts)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("未收敛: %+v", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// 收端重试同目录 → 第二次调用覆盖写完整 1024 字节
	paths, err := svc.PublishPaths(context.Background(), task.ID)
	if err != nil || len(paths) != 1 {
		t.Fatalf("发布路径: %v %v", paths, err)
	}
	if fi, _ := os.Stat(paths[0]); fi.Size() != 1024 {
		t.Fatalf("最终文件应 1024 字节: %d", fi.Size())
	}
	// 配额类失败不重试:由 TestQuotaReasonPreserved 覆盖（错误真因+终态语义一致）
}

// TestTaskLogExposure 日志环经 TaskLog 暴露(诊断面;stub 的 stdout/stderr 行齐活)。
func TestTaskLogExposure(t *testing.T) {
	bin := writeStub(t, "ok")
	svc := NewService(testDeps(t, bin))
	defer func() { _ = svc.Stop() }()

	cfg, _ := svc.deps.GetDirect(context.Background())
	src := filepath.Join(cfg.SendRoots[0], "logtest.txt")
	if err := os.WriteFile(src, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	task, err := svc.Send(context.Background(), []string{src})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var lines []string
	for time.Now().Before(deadline) {
		lines, err = svc.TaskLog(context.Background(), task.ID)
		if err != nil {
			t.Fatalf("TaskLog: %v", err)
		}
		if len(lines) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "口令") {
		t.Fatalf("日志环应含口令行, got: %q", joined)
	}
	if _, err := svc.TaskLog(context.Background(), "nonexistent"); err == nil {
		t.Fatal("不存在任务应报错")
	}
}

// TestStateFlipSemantics 状态语义真实化:公告就绪等单机自言自语不算「传输中」,
// 仅对端接触（PAKE 启动）或字节推进才离开 waiting。
func TestStateFlipSemantics(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "p2pc")
	body := `#!/bin/sh
case "$1" in
  send)
    echo "口令: ABCD-1234-WXYZ"
    echo "等待对端接入..."
    echo "公告就绪 base=http://r"
    echo "端口映射成功 external=1.2.3.4"
    sleep 30
    ;;
esac
`
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	svc := NewService(testDeps(t, bin))
	defer func() { _ = svc.Stop() }()

	cfg, _ := svc.deps.GetDirect(context.Background())
	src := filepath.Join(cfg.SendRoots[0], "sem.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	task, err := svc.Send(context.Background(), []string{src})
	if err != nil {
		t.Fatal(err)
	}
	// 等 stub 的三行输出被读完
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if lines, lerr := svc.TaskLog(context.Background(), task.ID); lerr == nil && len(lines) >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st := task.Snapshot().State; st != StateWaiting {
		t.Fatalf("公告就绪/端口映射不应翻 running, got %s", st)
	}
}
