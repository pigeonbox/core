package federation

// 联邦邀请定时同步的判据测试（core#32）。
//
// 判据逐条对应 issue 验收项：
//   - 周期可配（间隔）+ 可关（关掉就不跑）      → TestSyncConfigInterval / TestSyncRunDisabled
//   - 拉到的邀请正确落待收列表（形状一致）       → TestSyncerReceiveLandsInMailbox
//   - 调度器优雅退出（不留半截状态）             → TestSyncerRunRespectsContextCancel
//   - 重启后不重复推送（幂等）                   → TestSyncerRunIdempotent
//   - 失败有可读错误面、不静默吞掉               → TestSyncerRunPushErrorSurfaces

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSyncConfigInterval(t *testing.T) {
	tests := []struct {
		name string
		secs int
		want time.Duration
	}{
		{"零值取缺省", 0, DefaultSyncInterval},
		{"负值取缺省", -5, DefaultSyncInterval},
		{"正常值", 600, 10 * time.Minute},
		{"小于下界夹到下界", 5, minSyncInterval},
		{"等于下界", 60, time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SyncConfig{IntervalSeconds: tt.secs}.Interval()
			if got != tt.want {
				t.Fatalf("周期归一 = %v，期望 %v", got, tt.want)
			}
		})
	}
}

func TestSyncConfigNormalizedTargets(t *testing.T) {
	cfg := SyncConfig{Targets: []string{
		" https://a.example.com/ ", "https://a.example.com", "", "  ", "https://b.example.com//",
	}}
	got := cfg.NormalizedTargets()
	want := []string{"https://a.example.com", "https://b.example.com"}
	if len(got) != len(want) {
		t.Fatalf("目标归一 = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("目标归一 = %v，期望 %v", got, want)
		}
	}
}

func TestSyncConfigNormalizedCodes(t *testing.T) {
	cfg := SyncConfig{Codes: []string{" AAAA-BBBB-CCCC ", "AAAA-BBBB-CCCC", "", "DDDD-EEEE-FFFF"}}
	got := cfg.NormalizedCodes()
	want := []string{"AAAA-BBBB-CCCC", "DDDD-EEEE-FFFF"}
	if len(got) != len(want) {
		t.Fatalf("口令归一 = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("口令归一 = %v，期望 %v", got, want)
		}
	}
}

// TestSyncRunDisabled 关掉就不跑（不产生任何出站调用）。
func TestSyncRunDisabled(t *testing.T) {
	calls := 0
	s := NewSyncer(func(context.Context, string, string, string, string) error {
		calls++
		return nil
	}, nil)

	cfg := SyncConfig{
		Enabled: false,
		Targets: []string{"https://a.example.com"},
		Codes:   []string{"AAAA-BBBB-CCCC"},
	}
	if n := s.Run(context.Background(), cfg, "site", ""); n != 0 {
		t.Fatalf("关闭时推送数 = %d，期望 0", n)
	}
	if calls != 0 {
		t.Fatalf("关闭时出站调用 = %d，期望 0", calls)
	}
}

// TestSyncRunNoTargetsOrCodes 无目标或无口令时不推送。
func TestSyncRunNoTargetsOrCodes(t *testing.T) {
	calls := 0
	s := NewSyncer(func(context.Context, string, string, string, string) error {
		calls++
		return nil
	}, nil)

	if n := s.Run(context.Background(), SyncConfig{Enabled: true, Codes: []string{"A"}}, "s", ""); n != 0 {
		t.Fatalf("无目标时推送数 = %d，期望 0", n)
	}
	if n := s.Run(context.Background(), SyncConfig{Enabled: true, Targets: []string{"https://a"}}, "s", ""); n != 0 {
		t.Fatalf("无口令时推送数 = %d，期望 0", n)
	}
	if calls != 0 {
		t.Fatalf("出站调用 = %d，期望 0", calls)
	}
}

// TestSyncerRunIdempotent 同进程内周期重跑不重复推送（幂等闸之一）。
func TestSyncerRunIdempotent(t *testing.T) {
	type call struct{ target, code string }
	var calls []call
	s := NewSyncer(func(_ context.Context, target, code, _, _ string) error {
		calls = append(calls, call{target, code})
		return nil
	}, nil)

	cfg := SyncConfig{
		Enabled: true,
		Targets: []string{"https://a.example.com"},
		Codes:   []string{"AAAA-BBBB-CCCC"},
	}
	if n := s.Run(context.Background(), cfg, "site", ""); n != 1 {
		t.Fatalf("首轮推送数 = %d，期望 1", n)
	}
	// 第二轮（模拟下一周期）：不应重复推送
	if n := s.Run(context.Background(), cfg, "site", ""); n != 0 {
		t.Fatalf("次轮推送数 = %d，期望 0（幂等）", n)
	}
	if len(calls) != 1 {
		t.Fatalf("出站调用 = %d，期望 1（不得重复推送）", len(calls))
	}
}

// TestSyncerRunPushErrorSurfaces 推送失败有可读错误面：失败不静默（记日志），
// 且不影响其余组合；失败的不算已推送 ⇒ 下轮可重试。
func TestSyncerRunPushErrorSurfaces(t *testing.T) {
	boom := errors.New("对端返回 500")
	var okCalls, failCalls int
	s := NewSyncer(func(_ context.Context, target, _, _, _ string) error {
		if target == "https://bad.example.com" {
			failCalls++
			return boom
		}
		okCalls++
		return nil
	}, nil)

	cfg := SyncConfig{
		Enabled: true,
		Targets: []string{"https://bad.example.com", "https://good.example.com"},
		Codes:   []string{"AAAA-BBBB-CCCC"},
	}
	n := s.Run(context.Background(), cfg, "site", "")
	if n != 1 {
		t.Fatalf("推送成功数 = %d，期望 1（失败目标不影响其余）", n)
	}
	if failCalls != 1 || okCalls != 1 {
		t.Fatalf("调用数 fail=%d ok=%d，期望各 1", failCalls, okCalls)
	}
	// 失败组合未记入已推送 ⇒ 下轮会重试
	if s.SentCount() != 1 {
		t.Fatalf("已推送集合大小 = %d，期望 1（失败的不计入）", s.SentCount())
	}
}

// TestSyncerRunRespectsContextCancel 优雅退出：ctx 取消后不再推送（在飞轮次感知取消）。
func TestSyncerRunRespectsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	s := NewSyncer(func(context.Context, string, string, string, string) error {
		calls++
		cancel() // 第一轮内取消
		return nil
	}, nil)

	cfg := SyncConfig{
		Enabled: true,
		Targets: []string{"https://a.example.com", "https://b.example.com"},
		Codes:   []string{"AAAA-BBBB-CCCC", "DDDD-EEEE-FFFF"},
	}
	n := s.Run(ctx, cfg, "site", "")
	if n != 1 {
		t.Fatalf("取消后推送数 = %d，期望 1（取消后不再继续推送）", n)
	}
	if calls != 1 {
		t.Fatalf("出站调用 = %d，期望 1", calls)
	}
}

// TestSyncerReceiveLandsInMailbox 拉到的邀请正确落待收列表（与手动推送产生的条目形状一致）。
func TestSyncerReceiveLandsInMailbox(t *testing.T) {
	mailbox := NewInviteMailbox(nil)
	s := NewSyncer(nil, func(code, fromName, note string) error {
		_, err := mailbox.AddInvite(code, fromName, note)
		return err
	})

	const code = "AAAA-BBBB-CCCC"
	if err := s.Receive(code, "对端站", "附言"); err != nil {
		t.Fatalf("接收失败: %v", err)
	}
	list := mailbox.ListInvites()
	if len(list) != 1 {
		t.Fatalf("待收列表长度 = %d，期望 1", len(list))
	}
	got := list[0]
	if got.Code != code {
		t.Fatalf("待收条目 code = %q，期望 %q", got.Code, code)
	}
	if got.FromName != "对端站" || got.Note != "附言" {
		t.Fatalf("待收条目形状不一致: %+v", got)
	}
	if got.ExpiresIn <= 0 {
		t.Fatalf("待收条目剩余有效期 = %d，期望 > 0", got.ExpiresIn)
	}
	// 幂等：同码再收一次不新增条目
	if err := s.Receive(code, "对端站", "附言"); err != nil {
		t.Fatalf("重复接收失败: %v", err)
	}
	if len(mailbox.ListInvites()) != 1 {
		t.Fatalf("重复接收后长度 = %d，期望 1（同码去重）", len(mailbox.ListInvites()))
	}
}

// TestSyncerReceiveRejectsInvalidCode 非法口令有可读错误面（不静默吞掉）。
func TestSyncerReceiveRejectsInvalidCode(t *testing.T) {
	mailbox := NewInviteMailbox(nil)
	s := NewSyncer(nil, func(code, fromName, note string) error {
		_, err := mailbox.AddInvite(code, fromName, note)
		return err
	})
	err := s.Receive("not-a-code", "x", "")
	if err == nil {
		t.Fatal("非法口令应报错")
	}
	if !strings.Contains(err.Error(), "口令格式非法") {
		t.Fatalf("错误信息不可读: %v", err)
	}
}
