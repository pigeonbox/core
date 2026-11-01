package bootstrap

// 联邦邀请的定时同步装配（core#32）。
//
// 落点与约束：
//   - 调度器本体在 app/federation（sync.go）；本文件只做**装配**（依赖注入 +
//     后台任务起跑 + 配置热应用），与仓内其它后台任务（tasks.go / apikey_expiry.go）
//     同一范式。
//   - **只在 admin 面跑**（config.ServesAdminPlane()）：public 副本不跑 ⇒
//     多副本部署里只有 admin/standalone 实例驱动同步，N 个 public 副本不会
//     重复推送。
//   - **优雅退出**：复用 backgroundContext() 根 ctx（Cleanup 的 teardown 阶段
//     取消）+ trackBackgroundLoop（排水预算内等在飞轮次收尾）⇒ 进程关闭时
//     不留半截状态。

import (
	"context"

	"github.com/oekit/concurrency/loop"
	"go.uber.org/zap"

	configApp "github.com/pigeonbox/core/app/config"
	federationApp "github.com/pigeonbox/core/app/federation"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/logger"
)

// federationSyncConfig 读当前生效的同步配置（SystemConfig 的 p2p 段）。
//
// 读法与 PeerDirectory 的 registry provider 同源：读 **SystemConfig**
// （overlay + env 之后的生效值镜像，由配置域加锁保护），不直读全局 conf
// ——直读会与 overlay 的原地改写构成数据竞争。
//
// ⚠️ 用 DefaultSafe 而非 Default：Default 在未 InitDefault 时**panic**
// （fail-loud 装配错误语义）。本函数是「可选读取」——配置中心还没装配好
// （bootstrap 早期 / 单测）时，正确行为是**降级为不跑**（零行为），而不是
// 把整个进程炸掉。读不到 ⇒ 返回零值（Enabled=false）⇒ 调度器不起跑。
func federationSyncConfig(ctx context.Context) federationApp.SyncConfig {
	svc := configApp.DefaultSafe()
	if svc == nil {
		return federationApp.SyncConfig{}
	}
	cfg, err := svc.GetConfig(ctx)
	if err != nil || cfg == nil || cfg.P2P == nil || cfg.P2P.Federation == nil {
		return federationApp.SyncConfig{}
	}
	return federationApp.SyncConfig{
		Enabled:         cfg.P2P.Federation.Sync.Enabled,
		IntervalSeconds: cfg.P2P.Federation.Sync.IntervalSeconds,
		Targets:         cfg.P2P.Federation.Sync.Targets,
		Codes:           cfg.P2P.Federation.Sync.Codes,
	}
}

// federationSyncShouldRun 判定本副本**是否应当起跑**联邦定时同步。
//
// 抽成纯函数（配置读取与判定分离，可直接单测）——issue core#32 的硬约束是
// 「定时任务只在 admin 副本执行（public 副本不重复推送）」，这条约束必须
// **可判红**才能算落地。
//
// 判据（两条都满足才起跑）：
//  1. 本副本服务管理面（standalone / admin；public 副本**不跑**）；
//  2. 配置已启用（enabled=true ⇒ 零行为前提）。
//
// ⚠️ 两个条件都作为**入参**传入（而非函数内部去读配置中心）：读配置需要
// DB 支撑，单元测试里构造不出来；把「读」与「判」分开，判定逻辑才能被
// 逐档断言（public 档必须为假；admin 档在 enabled=true 时必须为真）。
//
// ⚠️ enabled 的来源：配置中心的生效值（overlay+env 之后）；读不到
// （配置中心未装配 / 出错）⇒ 视为未启用 ⇒ 不跑（缺配置 ≠ 崩溃，
// 但也不能假装有值——同 app/config.DefaultSafe 的口径）。
func federationSyncShouldRun(cfg *conf.AppConfiguration, enabled bool) bool {
	if cfg == nil || !cfg.ServesAdminPlane() {
		return false
	}
	return enabled
}

// startFederationSync 起跑联邦邀请的周期同步。
//
// 零行为前提：配置未启用（Enabled=false）⇒ 不起跑任何循环（不占资源、
// 不发任何出站请求）⇒ 与升级前行为完全一致。
func startFederationSync(mailbox *federationApp.InviteMailbox, siteName string) {
	// 只在 admin 面跑（standalone 与 admin 皆可；public 副本不跑），
	// 且配置已启用——判定集中在 federationSyncShouldRun（单一真相源，
	// 可单测判红）。
	if !federationSyncShouldRun(config, federationSyncConfig(context.Background()).Enabled) {
		return
	}
	// 首跑读配置：拿周期等参数（Enabled 已由上面的判定保证为 true）。
	initial := federationSyncConfig(context.Background())
	if !initial.Enabled {
		return
	}

	syncer := federationApp.NewSyncer(
		// 出站：复用既有推送实现（与手动 invite-push 同一链路 ⇒ 语义一致）。
		federationApp.PushInvite,
		// 入站：收进待收列表（收件箱同码去重 ⇒ 幂等）。
		func(code, fromName, note string) error {
			_, err := mailbox.AddInvite(code, fromName, note)
			return err
		},
	)

	trackBackgroundLoop(loop.Run(backgroundContext(), loop.Config{
		Name:       "federation-invite-sync",
		Interval:   initial.Interval(),
		FirstDelay: initial.Interval(),
	}, func(ctx context.Context) {
		// 每轮重读配置（周期可配 + 可关都是运行期语义：管理台改配置后
		// 下一轮即生效，无需重启）。
		cfg := federationSyncConfig(ctx)
		if !cfg.Enabled {
			return // 关掉就不跑（issue：周期可配 + 可关）
		}
		n := syncer.Run(ctx, cfg, siteName, "")
		if n > 0 {
			logger.Info("federation invite sync pushed",
				zap.Int("count", n),
				zap.Int("targets", len(cfg.NormalizedTargets())))
		}
	}))
}
