package bootstrap

import (
	"context"
	"testing"

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
