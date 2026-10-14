package p2px

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pigeonbox/core/conf"
)

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
	rootsDir := t.TempDir()  // 夹具目录只建一次——闭包内每次调用 t.TempDir()
	inboxDir := t.TempDir()  // 会产出新路径,收件区/白名单地址必须稳定
	return Deps{
		Standalone: func() bool { return true },
		DataDir:    t.TempDir(),
		MonitorInterval: 50 * time.Millisecond,
		GetDirect: func(ctx context.Context) (conf.P2PDirectConfig, bool) {
			return conf.P2PDirectConfig{
				Enabled:        true,
				RegistryURL:    "http://127.0.0.1:1",
				SendRoots:      []string{rootsDir},
				RecvDir:        inboxDir,
				MaxTaskBytes:   1024 * 1024, // 1MB
				InboxQuotaBytes: 10 * 1024 * 1024,
				BinaryPath:     binPath,
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

// ---- P3 邀请收发件箱 ----

// TestInviteInbox 收件箱:格式校验/去重/满箱/过期/认领出箱。
func TestInviteInbox(t *testing.T) {
	bin := writeStub(t, "ok")
	svc := NewService(testDeps(t, bin))
	defer func() { _ = svc.Stop() }()

	// 格式校验
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
	// 同码去重
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
	// 满箱
	for i := 0; i < 20; i++ {
		code := fmt.Sprintf("%04X-%04X-%04X", i, i, i)
		if _, err := svc.AddInvite(code, "", ""); err != nil {
			t.Fatalf("第%d 个入箱失败: %v", i, err)
		}
	}
	if _, err := svc.AddInvite("FFFF-FFFF-FFFF", "", ""); err == nil {
		t.Fatal("满箱应拒")
	}
	// 过期清扫(nowFn 注入拨快)
	base := time.Now()
	svc.nowFn = func() time.Time { return base.Add(11 * time.Minute) }
	if n := len(svc.ListInvites()); n != 0 {
		t.Fatalf("超 TTL 应清空: %d", n)
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
