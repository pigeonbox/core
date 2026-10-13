package p2px

import (
	"context"
	"os"
	"path/filepath"
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
	defer svc.Stop()

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
	defer svc.Stop()

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
	defer svc.Stop()
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
	defer svc.Stop()
	if _, err := svc.Send(context.Background(), []string{"/etc/passwd"}); err == nil {
		t.Fatal("白名单外路径应拒绝")
	}
}

// TestRecvDoneState 正常接收落盘即 done。
func TestRecvDoneState(t *testing.T) {
	bin := writeStub(t, "ok")
	svc := NewService(testDeps(t, bin))
	defer svc.Stop()
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
