package p2px

// 浏览器文件直发（upload.go）单测：进度行解析、上传授权模型（免 send_roots
// + 越界拒绝）、源体积估计、终态上传目录清理。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pigeonbox/core/conf"
)

func TestParseP2pcProgress(t *testing.T) {
	cases := []struct {
		line       string
		got, total int64
		ok         bool
	}{
		// slog TextHandler 形态（p2pcweb 子进程）
		{"time=2026-10-15T12:00:00.000+08:00 level=INFO msg=传输进度 got=1048576 total=10485760", 1048576, 10485760, true},
		{"time=... level=INFO msg=传输进度 got=0 total=0", 0, 0, true},
		// slog 默认 handler 形态（p2pc CLI 原样输出,无 msg= 前缀）
		{"2026/10/16 00:35:00 INFO 传输进度 got=1048576 total=6294356", 1048576, 6294356, true},
		{"2026/10/16 00:35:00 INFO 传输进度 got=42", 42, 0, true},
		{"time=... level=WARN msg=传输进度 got=42", 42, 0, true},
		{"· 传输通道建立  via=relay", 0, 0, false},
		{"msg=对端已接入,开始 PAKE", 0, 0, false},
		{"2026/10/16 00:35:00 INFO 已配对且身份核对通过,开始 PAKE", 0, 0, false},
		{"time=... msg=传输进度 got=abc total=5", 0, 5, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		got, total, ok := parseP2pcProgress(c.line)
		if ok != c.ok || got != c.got || total != c.total {
			t.Errorf("parseP2pcProgress(%q) = (%d,%d,%v), want (%d,%d,%v)",
				c.line, got, total, ok, c.got, c.total, c.ok)
		}
	}
}

// TestSendUploadedBypassesSendRoots 上传目录自带发送授权:send_roots 为空
// 也能发(常规 Send 同配置下会被拒),且任务带源体积估计;取消终态后上传
// 目录被自动清理。
func TestSendUploadedBypassesSendRoots(t *testing.T) {
	bin := writeStub(t, "ok")
	svc := NewService(testDeps(t, bin))
	defer func() { _ = svc.Stop() }()

	// 白名单置空:常规 Send 必须被拒(对照,证明上传直发的授权确实来自上传目录)
	base := svc.deps.GetDirect
	svc.deps.GetDirect = func(ctx context.Context) (conf.P2PDirectConfig, bool) {
		cfg, ok := base(ctx)
		cfg.SendRoots = nil
		return cfg, ok
	}

	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Send(context.Background(), []string{outside}); err == nil {
		t.Fatal("白名单外路径应被 Send 拒绝")
	}

	// 上传会话:写文件 → SendUploaded(不查 send_roots)
	dir, err := svc.NewUploadDir()
	if err != nil {
		t.Fatalf("NewUploadDir: %v", err)
	}
	src := filepath.Join(dir, "季度报表.pdf")
	payload := strings.Repeat("x", 4096)
	if err := os.WriteFile(src, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	task, err := svc.SendUploaded(context.Background(), dir, []string{src})
	if err != nil {
		t.Fatalf("SendUploaded: %v", err)
	}
	if snap := task.Snapshot(); snap.BytesTotal != int64(len(payload)) {
		t.Fatalf("BytesTotal 应为源体积 %d, got %d", len(payload), snap.BytesTotal)
	}
	// 等口令解析(证明子进程真被拉起)
	deadline := time.Now().Add(3 * time.Second)
	for task.Snapshot().Code == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if task.Snapshot().Code != "ABCD-1234-WXYZ" {
		t.Fatalf("口令解析失败: %q", task.Snapshot().Code)
	}
	if err := svc.Cancel(context.Background(), task.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	// 终态后上传目录应被清理
	deadline = time.Now().Add(3 * time.Second)
	for {
		if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("终态后 3s 上传目录未被清理")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestSendUploadedRejectsOutside 上传会话目录外的文件必须被拒(越界防线)。
func TestSendUploadedRejectsOutside(t *testing.T) {
	bin := writeStub(t, "ok")
	svc := NewService(testDeps(t, bin))
	defer func() { _ = svc.Stop() }()

	dir, err := svc.NewUploadDir()
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "evil.txt")
	if err := os.WriteFile(outside, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SendUploaded(context.Background(), dir, []string{outside}); err == nil {
		t.Fatal("会话目录外文件应被拒")
	}
	// 目录本身越界同样拒绝
	if _, err := svc.SendUploaded(context.Background(), t.TempDir(), []string{outside}); err == nil {
		t.Fatal("上传根外会话目录应被拒")
	}
	// 进程未被拉起(拒绝发生在 spawn 前)
	if n := len(svc.List(context.Background())); n != 0 {
		t.Fatalf("拒绝路径不应产生任务, got %d", n)
	}
}

// TestSweepUploadsOrphans 孤儿上传目录超龄清扫;在册活跃任务目录豁免。
func TestSweepUploadsOrphans(t *testing.T) {
	bin := writeStub(t, "ok")
	svc := NewService(testDeps(t, bin))
	defer func() { _ = svc.Stop() }()

	// 活跃任务的上传目录:置旧 mtime 也不许扫
	liveDir, err := svc.NewUploadDir()
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(liveDir, "a.txt")
	if err := os.WriteFile(src, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	task, err := svc.SendUploaded(context.Background(), liveDir, []string{src})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(liveDir, old, old); err != nil {
		t.Fatal(err)
	}
	// 无主孤儿目录:置旧后被扫
	orphan, err := svc.NewUploadDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}
	svc.sweepUploads()
	if _, err := os.Stat(liveDir); err != nil {
		t.Fatal("在册活跃任务的上传目录不应被扫")
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("超龄孤儿目录应被清扫")
	}
	_ = task
}
