// sync.go — 联邦邀请的定时同步（core#32）。
//
// 背景：联邦的「邀请推送」此前**只有手动触发**（POST /admin/p2p/invite-push
// ⇒ 实际 POST 对方站 /api/v1/p2p/invite），core/app/federation/ 下零调度面。
// 本文件补上**周期自动同步**面：定时把待推的邀请送给已配置的对端，并把
// 对端推来的邀请收进待收列表。
//
// ## 与既有语义一致（issue 硬要求：定时是补充不是替代）
//
// 手动推送走的是同一条链路（PushInvite → 对方站 POST /api/v1/p2p/invite），
// 本调度器只是**周期地**驱动同一份逻辑 ⇒ 手动端点保留且行为不变。
//
// ## 幂等（issue 硬要求：重启后不重复推送）
//
// 两道幂等闸：
//  1. 收侧：InviteMailbox.AddInvite 对同 code 去重（收件箱既有语义）。
//  2. 发侧：Sync 器自身维护「已推送过的 code」集合（内存态）⇒ 同一进程内
//     周期重跑不重复推送；进程重启后集合丢失，但收侧闸仍生效（对方站
//     收件箱同码去重）⇒ 端到端不重复。
//
// ## 前置事实（2026-11-01 核代码，纠正 issue 原文两处前提）
//
// ① issue 称「core/app/federation/ 下零 cron / Ticker」——**不成立**：
//
//	federation.go 的 Node.loop 已有 30min 心跳 ticker（注册续租 + 全量
//	重公告）。本文件加的是**邀请同步**调度面，与心跳面职责不同（心跳管
//	registry 租约，同步管邀请递送），不重复造轮子。
//
// ② issue 称「定时任务必须只在 admin 副本跑（否则 N 个副本重复推送）」——
//
//	**前提不成立**：conf.FederationAllowedInMode 规定联邦**只在 standalone
//	模式可用**（多副本下节点身份是进程级 Ed25519，语义未定义）⇒ 多副本
//	部署里联邦运行时根本不启动，不存在 N 副本重复推送。
//	但「只在 admin 面跑」仍是本调度器**必须**满足的约束（与仓内其它
//	后台任务同一道门）：public 副本不跑 ⇒ 也不依赖上面的架构性禁用。
//
// ## 依赖方向
//
// 本包不 import 生态任何兄弟模块（叶子约束，core CI dep guard）。
package federation

import (
	"context"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/pigeonbox/core/pkg/logger"
)

const (
	// DefaultSyncInterval 缺省同步周期（10min）。
	//
	// 取值理由：邀请信封 TTL 是 10min（inviteTTL）⇒ 周期与 TTL 同量级时，
	// 一个周期内推送的邀请在对端还有效；再长则对端收件箱已过期，重复推送
	// 无意义（白跑）。
	DefaultSyncInterval = 10 * time.Minute

	// minSyncInterval 同步周期下限（1min）——防配置把周期设成毫秒级，
	// 把对端打成推送风暴（每周期一次出站 HTTP）。
	minSyncInterval = time.Minute
)

// SyncConfig 联邦邀请定时同步配置（落在 SystemConfig 的 p2p 段）。
type SyncConfig struct {
	// Enabled 是否启用定时同步（默认 false ⇒ 零行为，与升级前一致）。
	Enabled bool `mapstructure:"enabled" json:"enabled"`

	// IntervalSeconds 同步周期（秒）。0 = 缺省 DefaultSyncInterval；
	// 小于 60 按 60 夹（minSyncInterval）。
	IntervalSeconds int `mapstructure:"interval_seconds" json:"interval_seconds"`

	// Targets 已配置的对端站点（每项一个基址，如 https://box.example.com）。
	//
	// 出站 URL **只来自本配置**（管理员显式登记），不是访客/调用方任意输入
	// ⇒ SSRF 面从「任意 URL」收窄到「admin 配的 URL」（与 core#29 跨站投递
	// 的信任模型同口径）。每个目标发请求前仍复跑 SSRF 校验。
	Targets []string `mapstructure:"targets" json:"targets"`

	// Codes 待推送的邀请口令（12 位字符 + 2 连字符，XXXX-XXXX-XXXX）。
	//
	// 语义：这些口令是**本站已创建、要递给对端**的取件码。调度器周期把它们
	// 推给 Targets 里的每个对端 ⇒ 对端访客用该码到对端柜取件。
	//
	// ⚠️ 熵门槛：联邦公告对低熵口令（6 位数字）是禁止出站的（见
	// announceAllowed）——但那是**registry 公告**面的约束；这里是把口令交给
	// **管理员显式登记的对端站**（口令 bearer 即授权，信任级同手工交接）。
	// 目标站地址由管理员配置 ⇒ 不是把口令散给任意第三方。
	Codes []string `mapstructure:"codes" json:"codes"`
}

// Interval 实际同步周期（配置归一：0⇒缺省，<下限⇒夹到下限）。
func (c SyncConfig) Interval() time.Duration {
	if c.IntervalSeconds <= 0 {
		return DefaultSyncInterval
	}
	d := time.Duration(c.IntervalSeconds) * time.Second
	if d < minSyncInterval {
		return minSyncInterval
	}
	return d
}

// NormalizedTargets 归一后的目标基址列表（去空白、去尾斜杠、去重、保序）。
func (c SyncConfig) NormalizedTargets() []string {
	seen := make(map[string]struct{}, len(c.Targets))
	out := make([]string, 0, len(c.Targets))
	for _, raw := range c.Targets {
		t := strings.TrimRight(strings.TrimSpace(raw), "/")
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// NormalizedCodes 归一后的口令列表（去空白、去重、保序）。
func (c SyncConfig) NormalizedCodes() []string {
	seen := make(map[string]struct{}, len(c.Codes))
	out := make([]string, 0, len(c.Codes))
	for _, raw := range c.Codes {
		code := strings.TrimSpace(raw)
		if code == "" {
			continue
		}
		if _, dup := seen[code]; dup {
			continue
		}
		seen[code] = struct{}{}
		out = append(out, code)
	}
	return out
}

// Syncer 联邦邀请的周期同步器。
//
// 依赖注入（便于测试与装配解耦）：
//   - push：把一个口令推到某个对端基址（生产用 PushInvite）。
//   - receive：把对端推来的口令收进待收列表（生产用 InviteMailbox.AddInvite）。
//
// 幂等：已推送的 (target, code) 组合记在 sent 集合里，同进程内不重复推送。
type Syncer struct {
	push    func(ctx context.Context, target, code, fromName, note string) error
	receive func(code, fromName, note string) error

	mu   sync.Mutex
	sent map[string]struct{} // "target|code" ⇒ 已推送
}

// NewSyncer 构造同步器（push/receive 任一为 nil ⇒ 该方向短路）。
func NewSyncer(
	push func(ctx context.Context, target, code, fromName, note string) error,
	receive func(code, fromName, note string) error,
) *Syncer {
	return &Syncer{
		push:    push,
		receive: receive,
		sent:    make(map[string]struct{}),
	}
}

// Run 执行一轮同步（ctx 感知取消：在飞轮次尽快收尾）。
//
// 返回本轮实际推送成功的条数（供日志与测试断言）。
func (s *Syncer) Run(ctx context.Context, cfg SyncConfig, fromName, note string) int {
	if !cfg.Enabled {
		return 0
	}
	targets := cfg.NormalizedTargets()
	codes := cfg.NormalizedCodes()
	if len(targets) == 0 || len(codes) == 0 {
		return 0
	}
	if s.push == nil {
		return 0
	}
	pushed := 0
	for _, target := range targets {
		for _, code := range codes {
			if ctx.Err() != nil {
				return pushed
			}
			key := target + "|" + code
			if s.alreadySent(key) {
				continue
			}
			if err := s.push(ctx, target, code, fromName, note); err != nil {
				// 单个目标失败不影响其余（issue：投递失败有可读错误面，
				// 不静默吞掉）——记日志后继续下一个组合。
				logger.Warn("federation sync push failed",
					zap.String("target", target),
					zap.Error(err))
				continue
			}
			s.markSent(key)
			pushed++
		}
	}
	return pushed
}

// Receive 收下对端推来的口令（落入待收列表）。
//
// 幂等由收件箱本身保证（同 code 去重）；这里仍返回错误面供上层判读。
func (s *Syncer) Receive(code, fromName, note string) error {
	if s.receive == nil {
		return nil
	}
	return s.receive(code, fromName, note)
}

// alreadySent 该 (target, code) 是否已推送过。
func (s *Syncer) alreadySent(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.sent[key]
	return ok
}

// markSent 标记 (target, code) 已推送。
func (s *Syncer) markSent(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent[key] = struct{}{}
}

// SentCount 已推送组合数（测试/观测用）。
func (s *Syncer) SentCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

// PushFunc 出站推送的函数签名（生产实现 = PushInvite）。
type PushFunc func(ctx context.Context, target, code, fromName, note string) error
