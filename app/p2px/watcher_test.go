package p2px

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pigeonbox/core/conf"
)

// ---- T4 信箱 watcher(stub 二进制,不依赖真实 p2pc/p2pd) ----

// writeWatcherStub 落一个假 p2pc 脚本(可执行)。
func writeWatcherStub(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// watcherService 构造带 discoverable 直传配置的服务（并注入联邦收件箱——
// 信箱 watcher 的产出只进收件箱，未装配时不拉起）。
func watcherService(t *testing.T, binPath string) *Service {
	t.Helper()
	return withFederation(NewService(Deps{
		Standalone:       func() bool { return true },
		DataDir:          t.TempDir(),
		MonitorInterval:  15 * time.Millisecond,
		NodeName:         "测试站",
		GhostPairTimeout: time.Minute,
		GetDirect: func(context.Context) (conf.P2PDirectConfig, bool) {
			return conf.P2PDirectConfig{
				Enabled:      true,
				Discoverable: true,
				BinaryPath:   binPath,
				RegistryURL:  "http://127.0.0.1:1", // stub 不拨号;真实链路由 p2p 仓 e2e 覆盖
			}, true
		},
	}))
}

func waitInvites(t *testing.T, s *Service, n int, timeout time.Duration) []InviteSnapshot {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		got := s.ListInvites()
		if len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	return s.ListInvites()
}

func TestWatcherFeedsInbox(t *testing.T) {
	// stub 吐 2 条邀请后长驻(不退出)
	bin := writeWatcherStub(t, "p2pc", `
echo '{"event":"invite","code":"AAAA-AAAA-AAAA","from":"aaaa","name":"发送站A","note":"附言一"}'
echo '{"event":"invite","code":"BBBB-BBBB-BBBB","from":"bbbb","name":"发送站B"}'
exec sleep 60
`)
	s := watcherService(t, bin)
	defer func() { _ = s.Stop() }()

	got := waitInvites(t, s, 2, 5*time.Second)
	if len(got) < 2 {
		t.Fatalf("watcher 事件应入收件箱 2 条,实得 %d: %+v", len(got), got)
	}
	codes := map[string]bool{}
	for _, inv := range got {
		codes[inv.Code] = true
	}
	if !codes["AAAA-AAAA-AAAA"] || !codes["BBBB-BBBB-BBBB"] {
		t.Fatalf("邀请口令不符: %+v", got)
	}
	// 事件里 name→FromName,note 透传
	found := false
	for _, inv := range got {
		if inv.Code == "AAAA-AAAA-AAAA" {
			found = true
			if inv.FromName != "发送站A" || inv.Note != "附言一" {
				t.Fatalf("邀请显示字段不符: %+v", inv)
			}
		}
	}
	if !found {
		t.Fatal("缺 AAAA 邀请")
	}
}

func TestWatcherRestartsAfterDeath(t *testing.T) {
	var mu sync.Mutex
	runs := 0
	// stub 每次启动吐一条唯一口令即退 → 守护按退避重启
	bin := writeWatcherStub(t, "p2pc", `
ID=$(printf '%04d' $(($$ % 10000)))
echo "{\"event\":\"invite\",\"code\":\"CCCC-$ID-CCCC\",\"from\":\"cccc\",\"name\":\"C\"}"
`)
	s := watcherService(t, bin)
	// 以收件数量间接度量重启轮数(同码去重,须收集 ≥2 个不同码=≥2 次进程)
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		runs = len(s.ListInvites())
		mu.Unlock()
		if runs >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if runs < 2 {
		t.Fatalf("watcher 进程死后应被守护重启(≥2 轮),实得 %d 轮邀请", runs)
	}
	_ = s.Stop()
}

func TestWatcherDisabledNoSpawn(t *testing.T) {
	// discoverable 关闭(名录 opt-in 缺省)= 期望态不满足,不得拉起进程
	bin := writeWatcherStub(t, "p2pc", `echo '{"event":"invite","code":"DDDD-DDDD-DDDD","from":"d"}'; exec sleep 60`)
	s := withFederation(NewService(Deps{
		Standalone:      func() bool { return true },
		DataDir:         t.TempDir(),
		MonitorInterval: 15 * time.Millisecond,
		GetDirect: func(context.Context) (conf.P2PDirectConfig, bool) {
			return conf.P2PDirectConfig{Enabled: true, Discoverable: false, BinaryPath: bin}, true
		},
	}))
	defer func() { _ = s.Stop() }()
	time.Sleep(150 * time.Millisecond) // 若误拉起,stub 会立即投喂
	if got := s.ListInvites(); len(got) != 0 {
		t.Fatalf("discoverable 关闭不得收邀请,实得 %d", len(got))
	}
	// 二进制缺失同样不拉起
	s2 := withFederation(NewService(Deps{
		Standalone:      func() bool { return true },
		DataDir:         t.TempDir(),
		MonitorInterval: 15 * time.Millisecond,
		GetDirect: func(context.Context) (conf.P2PDirectConfig, bool) {
			return conf.P2PDirectConfig{Enabled: true, Discoverable: true, BinaryPath: "/nonexistent/p2pc"}, true
		},
	}))
	defer func() { _ = s2.Stop() }()
	time.Sleep(150 * time.Millisecond)
	if got := s2.ListInvites(); len(got) != 0 {
		t.Fatalf("二进制缺失不得收邀请,实得 %d", len(got))
	}
}

func TestWatcherStopsWithService(t *testing.T) {
	bin := writeWatcherStub(t, "p2pc", `exec sleep 60`)
	s := watcherService(t, bin)
	waitFor(t, 2*time.Second, func() bool {
		s.watchMu.Lock()
		defer s.watchMu.Unlock()
		return s.watch.cmd != nil
	})
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	s.watchMu.Lock()
	gone := s.watch.cmd == nil && s.watch.pid == 0
	s.watchMu.Unlock()
	if !gone {
		t.Fatal("Stop 后 watcher 状态须清零")
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ---- 渗透回归:invite argv 注入闸(2026-10-15) ----

func TestSendRejectsMalformedInviteTo(t *testing.T) {
	bin := writeWatcherStub(t, "p2pc", `exec sleep 60`)
	root := t.TempDir() // 闭包外取一次:GetDirect 每次调用须返回同一目录
	s := NewService(Deps{
		Standalone:      func() bool { return true },
		DataDir:         t.TempDir(),
		MonitorInterval: 15 * time.Millisecond,
		GetDirect: func(context.Context) (conf.P2PDirectConfig, bool) {
			return conf.P2PDirectConfig{
				Enabled: true, BinaryPath: bin,
				SendRoots: []string{root},
			}, true
		},
	})
	defer func() { _ = s.Stop() }()
	real, err := filepath.EvalSymlinks(root) // macOS /var→/private/var,白名单归一同源
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(real, "x.txt")
	_ = os.WriteFile(src, []byte("x"), 0o644)

	for _, bad := range []string{"--key=/etc/passwd", "short", "XXXX", "a/b/c", strings.Repeat("a", 63) + "G"} {
		_, err := s.Send(context.Background(), []string{src}, WithInvite(bad, "--no-punch --out /tmp"))
		if err == nil || !strings.Contains(err.Error(), "invite_to") {
			t.Fatalf("invite_to=%q 应被拒绝: %v", bad, err)
		}
	}
	// 合法 hex64 通过
	if _, err := s.Send(context.Background(), []string{src}, WithInvite(strings.Repeat("a", 64), "note")); err != nil {
		t.Fatalf("合法 node_id 不应被拒: %v", err)
	}
}

func TestSanitizeInviteNote(t *testing.T) {
	got := sanitizeInviteNote("  a\x1ab\x7fc\n\td")
	if got != "abcd" {
		t.Fatalf("控制符应剥离且去首尾空白: %q", got)
	}
	long := sanitizeInviteNote(strings.Repeat("长", 300)) // 每字 3 字节
	if len(long) != 201 {                                // 写入后判停:上限 200 字节 + 截断时的最后一个 rune(3 字节)
		t.Fatalf("应限长 ~200 字节: %d", len(long))
	}
	if len(long)%3 != 0 {
		t.Fatalf("不应截断 UTF-8 rune: %d", len(long))
	}
}
