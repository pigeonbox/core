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
//   - **热启停**（对齐联邦运行时的热切换语义）：OnP2PFederationChanged 钩子
//     触发即按新配置起/停/换周期——管理台开启同步不用等重启进程，关闭即时停。
//   - **优雅退出**：循环挂在 backgroundContext() 根 ctx（Cleanup 的 teardown
//     阶段取消）+ trackBackgroundLoop（排水预算内等在飞轮次收尾）⇒ 进程关闭时
//     不留半截状态；热停走同一取消路径（在飞轮次感知 ctx 自行收尾）。

import (
	"context"
	"sync"
	"time"

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
	return syncConfigFromConf(cfg.P2P.Federation.Sync)
}

// syncConfigFromConf conf 段 → app 域同步配置（热路径与读库路径共用）。
func syncConfigFromConf(f conf.FederationSyncConfig) federationApp.SyncConfig {
	return federationApp.SyncConfig{
		Enabled:         f.Enabled,
		IntervalSeconds: f.IntervalSeconds,
		Targets:         f.Targets,
		Codes:           f.Codes,
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

// federationSyncScheduler 周期循环的生命周期管理（热启停）。
//
// 语义对齐联邦运行时的热切换（fedRT.apply）：enabled=false ⇒ 停；
// enabled=true ⇒ 未跑则起、周期变了则重启换周期；已在跑且周期未变 ⇒ 不动
// （轮次内每轮自读配置，改动下一轮即见）。
//
// 测试缝：round 为每轮动作（生产 = 读配置 + Syncer.Run；测试注入计数器），
// interval 由调用方给（生产传 SyncConfig.Interval() 的夹取值，测试传毫秒级）。
type federationSyncScheduler struct {
	mu       sync.Mutex
	cancel   context.CancelFunc
	loop     *loop.Loop
	interval time.Duration
	running  bool
	round    func(ctx context.Context)
}

// fedSyncSched 包级单例（initThriftIDLServices 装配；停机随 backgroundContext
// teardown——循环是其子 ctx，取消即优雅退出）。
var fedSyncSched = &federationSyncScheduler{}

// apply 按新配置起/停/重启周期循环（热启停；串行化）。
func (s *federationSyncScheduler) apply(enabled bool, interval time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !enabled || s.round == nil {
		s.stopLocked()
		return
	}
	if s.running && s.interval == interval {
		return // 已在跑且周期未变：不动（轮次内每轮自读配置）
	}
	s.stopLocked()
	ctx, cancel := context.WithCancel(backgroundContext())
	l := loop.Run(ctx, loop.Config{
		Name:       "federation-invite-sync",
		Interval:   interval,
		FirstDelay: interval,
	}, s.round)
	s.cancel, s.loop = cancel, l
	s.interval, s.running = interval, true
	// 挂进排水 WaitGroup：进程停机时在飞轮次收尾后再退出（优雅停）。
	trackBackgroundLoop(l)
}

// stop 热停（取消循环 ctx；在飞轮次感知取消自行收尾，排水由
// trackBackgroundLoop 兜底）。
func (s *federationSyncScheduler) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
}

func (s *federationSyncScheduler) stopLocked() {
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.loop = nil
	s.running = false
}

// startFederationSync 装配每轮动作并按当前配置起跑（零行为前提：配置未启用
// ⇒ 不起跑任何循环、不发任何出站请求，与升级前行为一致）。
func startFederationSync(mailbox *federationApp.InviteMailbox, siteName string) {
	syncer := federationApp.NewSyncer(
		// 出站：按对端粒度 SSRF 校验的推送（与手动 invite-push 同一链路
		// ⇒ 语义一致；私网放行仅限登记对端的 allow_private）。
		federationApp.PushInviteToTarget,
		// 入站：收进待收列表（收件箱同码去重 ⇒ 幂等）。
		func(code, fromName, note string) error {
			_, err := mailbox.AddInvite(code, fromName, note)
			return err
		},
	)
	fedSyncSched.mu.Lock()
	fedSyncSched.round = func(ctx context.Context) {
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
	}
	fedSyncSched.mu.Unlock()
	federationSyncApply(federationSyncConfig(context.Background()))
}

// federationSyncApply 按同步配置热启停（启动路径与热路径共用；判定集中在
// federationSyncShouldRun 单一真相源）。
func federationSyncApply(cfg federationApp.SyncConfig) {
	if !federationSyncShouldRun(config, cfg.Enabled) {
		fedSyncSched.stop()
		return
	}
	fedSyncSched.apply(cfg.Enabled, cfg.Interval())
}

// federationSyncApplyFromConf 热路径入口（OnP2PFederationChanged 钩子）：
// 管理台保存 p2p.federation 段（含 sync 子段）即起/停/换周期，无需重启进程。
func federationSyncApplyFromConf(f conf.FederationConfig) {
	federationSyncApply(syncConfigFromConf(f.Sync))
}
