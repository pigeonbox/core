package bootstrap

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pigeonbox/core/conf"
)

// TestFederationSyncShouldRunAdminPlaneOnly core#32 硬约束：**定时任务只在 admin
// 副本执行**（public 副本不重复推送）。
//
// 背景：多副本部署（public×N + admin×1）下若 public 副本也跑同步 ⇒ N 个副本
// 周期重复推送同一批邀请 ⇒ 对端柜被重复投递（收件箱虽有去重，但仍是多余的
// 出站请求与错误面）。
//
// 判据形状：**逐档断言**（3 个部署模式 × 启用/未启用 两种状态），不是只看
// 整体绿。public 档必须恒为假；standalone/admin 档由开关决定。
// ⚠️ 若实现漏掉 public 档的判定，这一档会从「假」变「真」⇒ 立刻转红。
func TestFederationSyncShouldRunAdminPlaneOnly(t *testing.T) {
	cases := []struct {
		name    string
		mode    string
		enabled bool
		want    bool
	}{
		{"public 副本 · 未启用", conf.DeployModePublic, false, false},
		{"public 副本 · 已启用（关键档）", conf.DeployModePublic, true, false},
		{"standalone · 未启用", conf.DeployModeStandalone, false, false},
		{"standalone · 已启用", conf.DeployModeStandalone, true, true},
		{"admin · 未启用", conf.DeployModeAdmin, false, false},
		{"admin · 已启用", conf.DeployModeAdmin, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &conf.AppConfiguration{}
			cfg.Deployment.Mode = c.mode
			got := federationSyncShouldRun(cfg, c.enabled)
			if got != c.want {
				t.Fatalf("mode=%s enabled=%v 起跑判定错误: got=%v want=%v",
					c.mode, c.enabled, got, c.want)
			}
		})
	}

	// nil 配置（装配未完成）⇒ 不跑（缺配置 ≠ 崩溃，也不能假装有值）。
	if federationSyncShouldRun(nil, true) {
		t.Fatal("nil 配置不应起跑")
	}
}

// TestStartFederationSyncNoPanic 配置中心未装配时不得 panic。
//
// 背景：startFederationSync 读配置走 app/config.DefaultSafe（未 InitDefault
// 返回 nil 而非 panic）——判据是「读不到 ⇒ 降级为不跑」，不是「把进程炸掉」。
// ⚠️ 若实现改回 Default()（fail-loud panic 语义），本测试立刻转红。
func TestStartFederationSyncNoPanic(t *testing.T) {
	old := config
	t.Cleanup(func() { config = old })

	config = &conf.AppConfiguration{}
	config.Deployment.Mode = conf.DeployModeStandalone
	// nil mailbox：若实现错误地起了跑（走到 AddInvite）会 nil 解引用 panic。
	startFederationSync(nil, "site")
}

// TestFederationSyncConfigZeroValue core#32 零行为前提：配置域读不到时
// federationSyncConfig 必须返回零值（Enabled=false）⇒ 调度器不起跑。
func TestFederationSyncConfigZeroValue(t *testing.T) {
	cfg := federationSyncConfig(context.Background())
	if cfg.Enabled {
		t.Fatal("未配置时 federationSyncConfig 应为零值（Enabled=false）")
	}
	if len(cfg.Targets) != 0 || len(cfg.Codes) != 0 {
		t.Fatalf("未配置时不应有目标或口令: targets=%v codes=%v", cfg.Targets, cfg.Codes)
	}
}

// waitForPoll 轮询等待条件成立（毫秒级周期测试用；避免裸 sleep 竞态判红）。
func waitForPoll(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时（%v）: %s", timeout, what)
}

// TestFederationSyncSchedulerTickHotStopHotStart 调度器三判据：
//   - **定时触发**：起跑后按周期反复执行轮次（不是只跑一次）；
//   - **热停**：apply(false) 后轮次不再发生，且循环排水退出（优雅停）；
//   - **热启**：关掉再开（管理台改配置的热路径形态）轮次恢复——
//     无需重启进程（对齐联邦运行时热切换语义）。
//
// ⚠️ 负对照（改错转红，PR 实测）：apply 不起循环 ⇒「定时触发」转红；
// stopLocked 不 cancel ⇒「优雅停」转红；apply 不支持重启 ⇒「热启」转红。
func TestFederationSyncSchedulerTickHotStopHotStart(t *testing.T) {
	var n atomic.Int64
	s := &federationSyncScheduler{round: func(context.Context) { n.Add(1) }}

	// ① 定时触发：15ms 周期，2s 内至少跑满 2 轮
	s.apply(true, 15*time.Millisecond)
	waitForPoll(t, 2*time.Second, func() bool { return n.Load() >= 2 }, "周期轮次未触发")

	// ② 热停 + 优雅停：拿当前循环的 Done，停后它必须排水退出，且轮次归零增长
	s.mu.Lock()
	l := s.loop
	s.mu.Unlock()
	if l == nil {
		t.Fatal("起跑后应有在跑循环")
	}
	s.apply(false, 0)
	select {
	case <-l.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("热停后循环未退出（优雅停判据：取消 ctx 后循环必须收尾）")
	}
	stopped := n.Load()
	time.Sleep(150 * time.Millisecond)
	if got := n.Load(); got != stopped {
		t.Fatalf("热停后仍有轮次触发: %d ⇒ %d", stopped, got)
	}

	// ③ 热启：再次开启（周期同 ①）轮次恢复
	base := n.Load()
	s.apply(true, 15*time.Millisecond)
	waitForPoll(t, 2*time.Second, func() bool { return n.Load() > base }, "热启后轮次未恢复")
	s.stop()
}

// TestFederationSyncSchedulerRestartOnIntervalChange 周期变了要换周期重启：
// 先挂长周期（1h，200ms 内必然 0 轮），再改短周期 ⇒ 轮次开始发生。
// （已跑且周期不变时不动——由 ① 的连续触发间接覆盖。）
//
// ⚠️ 负对照：apply 对「在跑 + 周期变化」不重启 ⇒ 本测试转红。
func TestFederationSyncSchedulerRestartOnIntervalChange(t *testing.T) {
	var n atomic.Int64
	s := &federationSyncScheduler{round: func(context.Context) { n.Add(1) }}

	s.apply(true, time.Hour)
	time.Sleep(200 * time.Millisecond)
	if got := n.Load(); got != 0 {
		t.Fatalf("长周期不应在 200ms 内触发: got=%d", got)
	}

	s.apply(true, 15*time.Millisecond) // 换周期 ⇒ 重启
	waitForPoll(t, 2*time.Second, func() bool { return n.Load() >= 1 }, "换周期后未按新周期触发")
	s.stop()
}
