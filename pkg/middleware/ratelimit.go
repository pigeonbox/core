// Package middleware 提供 Hertz 中间件：
//   - RateLimit: IP 维度 + 接口维度双层限流
//
// 实现：默认进程内 token bucket——per-IP 桶由 oekit/concurrency/ratelimit 的
// KeyedLimiter 托管（工厂造桶 + lastSeen TTL 自动清死桶，底层 x/time/rate）；
// 配置 rate_limit.use_redis=true 且 Redis 可用时切换为 Redis 固定窗口计数
// （多实例共享；窗口判定收编 oekit/redisx 的 PeriodLimit——Lua 原子
// INCRBY+首次 EXPIRE + 三态返回，本仓只留配置映射与三态降级口径）。
// 命中限流后按 block_seconds 封禁该 IP+维度（封禁同样 Redis/内存双写）。
// 配置可经 /admin/ratelimit/config 运行时热更。
package middleware

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/oekit/concurrency/loop"
	"github.com/oekit/concurrency/ratelimit"
	redisxratelimit "github.com/oekit/redisx/ratelimit"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"golang.org/x/time/rate"

	"github.com/pigeonbox/contracts/errcode"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/pkg/metrics"
	"github.com/pigeonbox/core/pkg/resp"
)

// RateLimitConfig 限流配置（运行时可调）
type RateLimitConfig struct {
	GlobalQPS    int  // 全局 IP 维度 QPS（每 IP）
	UploadQPS    int  // 上传接口 QPS
	DownloadQPS  int  // 下载接口 QPS
	LoginQPS     int  // 登录接口 QPS
	Burst        int  // 突发容量
	Enabled      bool // 总开关
	BlockSeconds int  // 触发限流后的封禁秒数（0 = 不封禁）
	UseRedis     bool // Redis 固定窗口模式（多实例共享）
}

// DefaultRateLimitConfig 默认限流配置
func DefaultRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{
		GlobalQPS:    100,
		UploadQPS:    10,
		DownloadQPS:  50,
		LoginQPS:     5,
		Burst:        20,
		Enabled:      true,
		BlockSeconds: 60,
	}
}

// RateLimitConfigFromConf 从全局配置读取（未配置段时用默认值）
func RateLimitConfigFromConf() RateLimitConfig {
	cfg := DefaultRateLimitConfig()
	c := conf.GetGlobalConfig()
	if c == nil {
		return cfg
	}
	rl := c.RateLimit
	if rl.GlobalQPS > 0 {
		cfg.GlobalQPS = rl.GlobalQPS
	}
	if rl.UploadQPS > 0 {
		cfg.UploadQPS = rl.UploadQPS
	}
	if rl.DownloadQPS > 0 {
		cfg.DownloadQPS = rl.DownloadQPS
	}
	if rl.LoginQPS > 0 {
		cfg.LoginQPS = rl.LoginQPS
	}
	if rl.Burst > 0 {
		cfg.Burst = rl.Burst
	}
	if rl.BlockSeconds > 0 {
		cfg.BlockSeconds = rl.BlockSeconds
	}
	if rl.Enabled != nil {
		cfg.Enabled = *rl.Enabled
	}
	cfg.UseRedis = rl.UseRedis
	return cfg
}

// scope 限流维度
type scope string

const (
	scopeGlobal   scope = "global"
	scopeUpload   scope = "upload"
	scopeDownload scope = "download"
	scopeLogin    scope = "login"
)

func (s scope) qpsOf(cfg RateLimitConfig) int {
	switch s {
	case scopeUpload:
		return cfg.UploadQPS
	case scopeDownload:
		return cfg.DownloadQPS
	case scopeLogin:
		return cfg.LoginQPS
	default:
		return cfg.GlobalQPS
	}
}

// limiterIdleTTL 单 IP 桶空闲淘汰线（KeyedLimiter 内部每 ttl/2 清扫一轮）。
// 沿用旧手搓 GC 的 10min 淘汰语义。
const limiterIdleTTL = 10 * time.Minute

// RateLimiter 限流管理器
type RateLimiter struct {
	// mu 为读写锁：cfg 只由 UpdateConfig（写锁）整体替换，热路径只读快照
	// ⇒ 读侧用 RLock 才能既无竞态又不让限流串行化。
	mu sync.RWMutex
	// limiters 各 scope 的 per-IP 桶托管（oekit KeyedLimiter：TTL 自动清死桶，
	// 替代旧手搓双层 map + lastAccess 追踪 + GC 协程）
	limiters map[scope]*ratelimit.KeyedLimiter
	blocked  map[string]time.Time // "scope|ip" → 封禁截止
	cfg      RateLimitConfig
	rdb      *redisCmds
	gcCancel context.CancelFunc // 停封禁表清扫循环（loop.Run 的 ctx 生命周期）

	// —— allowRedis 的组合判定器缓存（redisx.ResilientLimiter，per-scope 实例）——
	// 专用小锁：热路径只碰这张小 map，不占 rl.mu（避免与配置快照的读写锁
	// 争用，也避免与 getLimiter 的写锁反序嵌套）。
	periodMu       sync.Mutex
	periodLimiters map[scope]*resilientEntry

	blockedTotal int64 // 累计封禁次数（观测用）
}

// NewRateLimiter 创建限流管理器
func NewRateLimiter(cfg RateLimitConfig) *RateLimiter {
	rl := &RateLimiter{
		limiters:       map[scope]*ratelimit.KeyedLimiter{},
		blocked:        map[string]time.Time{},
		cfg:            cfg,
		periodLimiters: map[scope]*resilientEntry{},
	}
	rl.rebuildLimiters(cfg)
	// 后台定期清理封禁表（client 桶的空闲清理由 KeyedLimiter 自管，见 gc）；
	// loop.Run 托管生命周期（ctx 取消可打断+RunSafeLabeled panic 归因+Wait 排水）
	gcCtx, cancel := context.WithCancel(context.Background())
	rl.gcCancel = cancel
	loop.Run(gcCtx, loop.Config{Interval: 5 * time.Minute, FirstDelay: 5 * time.Minute, Name: "ratelimit.gc"},
		func(ctx context.Context) { rl.gc() })
	return rl
}

// rebuildLimiters 按 cfg 建 4 个 scope 的 per-IP 桶托管（重建前 Close 旧实例，
// 停其自管清理协程）。工厂闭包捕获造桶当时的 qps/burst、不回头读 rl.cfg：
// 热更走整体重建（UpdateConfig），与旧实现「清空现有 limiter 让新配置生效」
// 同语义；且工厂锁自由，避免 KeyedLimiter 内部锁与 rl.mu 反序嵌套死锁。
func (rl *RateLimiter) rebuildLimiters(cfg RateLimitConfig) {
	for _, s := range []scope{scopeGlobal, scopeUpload, scopeDownload, scopeLogin} {
		qps := s.qpsOf(cfg)
		if qps <= 0 {
			qps = 1
		}
		burst := cfg.Burst
		if burst <= 0 {
			burst = qps
		}
		if old := rl.limiters[s]; old != nil {
			old.Close()
		}
		rl.limiters[s] = ratelimit.NewKeyedLimiter(func() ratelimit.Limiter {
			return ratelimit.NewTokenBucket(rate.Limit(qps), burst)
		}, limiterIdleTTL)
	}
}

// SetRedis 注入 Redis（use_redis=true 时启用分布式固定窗口）
func (rl *RateLimiter) SetRedis(rdb *redis.Client) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	// 包一层（纯包装：不新建连接、不改连接参数）；redis 未装配时保持 nil
	// ⇒ 内存桶回落路径不被破坏。
	if rdb == nil {
		rl.rdb = nil
		return
	}
	rl.rdb = newRedisCmds(rdb)
}

// UpdateConfig 热更新配置
func (rl *RateLimiter) UpdateConfig(cfg RateLimitConfig) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.cfg = cfg
	// 整体重建，让新配置即刻生效（同旧实现的清空语义）
	rl.rebuildLimiters(cfg)
}

// Stop 停止封禁表清扫循环与各 scope 桶托管的自管清理
func (rl *RateLimiter) Stop() {
	rl.gcCancel()
	rl.mu.Lock()
	defer rl.mu.Unlock()
	for _, kl := range rl.limiters {
		kl.Close()
	}
}

// gc 清理封禁表过期条目。per-IP 桶的空闲淘汰已由 KeyedLimiter 自管
// （limiterIdleTTL，内部每 ttl/2 清扫一轮），此处不再过手。
func (rl *RateLimiter) gc() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	for k, until := range rl.blocked {
		if until.Before(time.Now()) {
			delete(rl.blocked, k)
		}
	}
}

// snapshotCfg 取配置快照。**cfg 的一切读取都必须经此**，不得直接读 rl.cfg。
//
// 2026-11-06 修数据竞态：cfg 在 UpdateConfig 里持锁写，而 allow / addBlock /
// allowRedis 此前**无锁读** ⇒ 与配置热更并发时是 data race（-race 可复现），
// 且可能读到「cfg 已是新值、limiters 还是旧桶」的撕裂态（新 qps 配旧桶 ⇒
// 限流值短暂不符，排查时极难归因）。改快照后写入仍是整体替换，语义不变。
func (rl *RateLimiter) snapshotCfg() RateLimitConfig {
	rl.mu.RLock()
	defer rl.mu.RUnlock()
	return rl.cfg
}

// snapshotRedis 取 Redis 句柄快照（SetRedis 持写锁替换 ⇒ 读侧必须经此）。
// 与 cfg 同一竞态面：「rl.rdb 判空 + 使用」这种写法在装配期 SetRedis 与
// 流量并发时同样是 data race。
func (rl *RateLimiter) snapshotRedis() *redisCmds {
	rl.mu.RLock()
	defer rl.mu.RUnlock()
	return rl.rdb
}

// getLimiter 获取（或由 KeyedLimiter 惰性创建）某 IP 在某 scope 下的桶
func (rl *RateLimiter) getLimiter(s scope, ip string) ratelimit.Limiter {
	rl.mu.Lock()
	kl := rl.limiters[s]
	rl.mu.Unlock()
	if kl == nil {
		return nil
	}
	return kl.Get(ip)
}

// blockKey 封禁/Redis 计数的组合键
func blockKey(s scope, ip string) string { return string(s) + "|" + ip }

// isBlocked 检查（内存 + Redis 双层）
func (rl *RateLimiter) isBlocked(ctx context.Context, s scope, ip string) bool {
	key := blockKey(s, ip)
	if rdb := rl.snapshotRedis(); rdb != nil {
		if n, err := rdb.Exists(ctx, "pb:rl:block:"+key).Result(); err == nil && n > 0 {
			return true
		}
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	until, ok := rl.blocked[key]
	return ok && until.After(time.Now())
}

// addBlock 写入封禁（内存 + Redis，取两者较长）
func (rl *RateLimiter) addBlock(ctx context.Context, s scope, ip string) {
	secs := rl.snapshotCfg().BlockSeconds
	if secs <= 0 {
		return
	}
	key := blockKey(s, ip)
	until := time.Now().Add(time.Duration(secs) * time.Second)
	rl.mu.Lock()
	rl.blocked[key] = until
	rl.blockedTotal++
	rl.mu.Unlock()
	if rdb := rl.snapshotRedis(); rdb != nil {
		rdb.Set(ctx, "pb:rl:block:"+key, "1", time.Duration(secs)*time.Second)
	}
}

// redisWindowPeriod Redis 固定窗口时长（与旧手搓版「按 epoch 秒切窗」同窗宽）。
const redisWindowPeriod = time.Second

// redisCountKeyPrefix Redis 计数键前缀（scope 段）。PeriodLimit 内部自拼 ":"
// ⇒ 最终键 "pb:rl:cnt:<scope>:<ip>"——与旧手搓键
// "pb:rl:cnt:<scope>:<ip>:<unix>" 同前缀；窗口段从键后缀挪进 TTL 后尾段
// 不再需要，旧键 2s TTL 自然过期，无迁移问题。
func redisCountKeyPrefix(s scope) string { return "pb:rl:cnt:" + string(s) }

// resilientEntry 缓存的组合判定器 + 构建时快照（quota/client）。
// 任一快照与当前不符 ⇒ 下一次判定前惰性重建（配额热更 / SetRedis 换端）。
type resilientEntry struct {
	res    *redisxratelimit.ResilientLimiter
	quota  int64         // 构建时的配额（s.qpsOf）
	client *redis.Client // 构建时的 store（redis.Scripter 的具体载体）
}

// localAdapter 把 concurrency ratelimit.Limiter（Allow() bool，TokenBucket 的
// 接口形态）适配到 redisx LocalLimiter（AllowN(now, n)——与 x/time/rate 同形）。
// 忽略 now/n（桶自身管时钟与令牌数），只保留放行判定。
type localAdapter struct{ l ratelimit.Limiter }

func (a localAdapter) AllowN(now time.Time, n int) bool { return a.l.Allow() }

// observeFallback 构造 ResilientLimiter 的降级观测闭包：计数（metrics 恒增）
// + 节流日志（Redis 故障期间每次请求都降级，Warn 60s 至多一条，高频面交给
// 计数器承担——core#83 阶段 2 的新观测面：降级不再静默）。
func observeFallback(s scope) func(redisxratelimit.Source) {
	var lastWarn atomic.Int64
	return func(src redisxratelimit.Source) {
		metrics.RecordRatelimitFallback(string(s), src.String())
		now := time.Now().Unix()
		if prev := lastWarn.Load(); prev >= now-60 {
			return // 60s 内已打过
		} else if !lastWarn.CompareAndSwap(prev, now) {
			return // 并发路径已有人打过
		}
		logger.Warn("[ratelimit] Redis 不可用，限流降级本地桶判定",
			zap.String("scope", string(s)),
			zap.String("source", src.String()))
	}
}

// resilientFor 取（或惰性构建）某 scope 的组合判定器（Redis 窗口 + 本地桶
// 兜底，redisx #118 的 ResilientLimiter——阶段 2 收编：三态降级口径与来源
// 观测上收上游单源，本仓只留配置映射与观测接线）。
//
// 阶段 1 手拼的「Unknown ⇒ getLimiter().Allow()」降级分支被上游
// decideFallback 等价接管；语义对照（阶段 1 用例全部保留作回归锁）：
//   - Allowed/HitQuota ⇒ 放行；OverQuota ⇒ 拒（addBlock 双层封禁不动）；
//   - Unknown（Redis 故障/ctx 超时）且有本地桶 ⇒ SourceLocalFallback 按桶判
//     （与手拼版 getLimiter().Allow() 同一批 KeyedLimiter per-IP 桶）；
//   - Unknown 且桶缺失（scope 未注册，理论不可达）⇒ FailModeOpen 放行
//     （与手拼版 `limiter == nil || limiter.Allow()` 的 nil→true 同口径）；
//   - Redis 恢复后下一次判定自动切回 SourceRedis（上游无探活后台，故障期
//     每请求多付一次失败往返，与阶段 1 「缓存实例保留」同自愈口径）。
//
// PeriodLimit 是 per-实例 quota（构造参数），故按 scope 各持一个实例；构造期
// store 未就绪（SetRedis 后到）⇒ 惰性构建 + 缓存。配额热更（UpdateConfig 换
// qps）或 SetRedis 换客户端 ⇒ 快照失配、下一次判定前重建——两次判定之间不
// 存在窗口，惰性重建与「同步重建」无行为差异。
func (rl *RateLimiter) resilientFor(s scope, quota int64, client *redis.Client) *redisxratelimit.ResilientLimiter {
	rl.periodMu.Lock()
	defer rl.periodMu.Unlock()
	if e := rl.periodLimiters[s]; e != nil && e.quota == quota && e.client == client {
		return e.res
	}
	dist := redisxratelimit.NewPeriodLimit(redisWindowPeriod, quota, client, redisCountKeyPrefix(s),
		redisxratelimit.WithPeriodAlign())
	res := redisxratelimit.NewResilientLimiter(dist, func(key string) redisxratelimit.LocalLimiter {
		if l := rl.getLimiter(s, key); l != nil {
			return localAdapter{l: l}
		}
		return nil
	}, redisxratelimit.WithFallbackMetrics(observeFallback(s)))
	rl.periodLimiters[s] = &resilientEntry{res: res, quota: quota, client: client}
	return res
}

// allowRedis Redis 固定窗口计数（1s 窗口）——阶段 1 窗口判定收编 PeriodLimit
// 后，阶段 2 再收编 ResilientLimiter（Redis 窗口 + 本地桶兜底组合原语）：
// 三态降级口径与「判定来源」观测均由上游单源承担（语义对照表见 resilientFor），
// 本方法只剩配置映射（qps→quota）与放行判定。
//
// 窗口对齐：WithPeriodAlign 按自然周期对齐（与旧 epoch 秒键同款语义）。
// 1s 窗口下上游对齐/不对齐的 TTL 计算两式恰好重合（period - unix%period ==
// period），当天行为无差；接线该选项是为窗口将来调大时保持 epoch 对齐、
// 不让窗口起点随首个请求漂移。
func (rl *RateLimiter) allowRedis(ctx context.Context, rdb *redisCmds, s scope, ip string) bool {
	qps := s.qpsOf(rl.snapshotCfg())
	if qps <= 0 {
		qps = 1
	}
	res := rl.resilientFor(s, int64(qps), rdb.Client)
	d, _ := res.Allow(ctx, ip) // 降级判定已在组合原语内完成；err 仅伴随非 Redis Source
	return d.Allowed
}

// allow 检查 IP 在某 scope 下是否被允许
func (rl *RateLimiter) allow(ctx context.Context, s scope, ip string) bool {
	cfg := rl.snapshotCfg()
	if !cfg.Enabled {
		return true
	}
	if rl.isBlocked(ctx, s, ip) {
		return false
	}
	var ok bool
	if rdb := rl.snapshotRedis(); cfg.UseRedis && rdb != nil {
		ok = rl.allowRedis(ctx, rdb, s, ip)
	} else {
		limiter := rl.getLimiter(s, ip)
		ok = limiter == nil || limiter.Allow()
	}
	if !ok {
		rl.addBlock(ctx, s, ip)
	}
	return ok
}

// SnapshotConfig 返回当前配置副本（并发安全，admin 接口用）
func (rl *RateLimiter) SnapshotConfig() RateLimitConfig {
	rl.mu.RLock()
	defer rl.mu.RUnlock()
	return rl.cfg
}

// ProbeAllow 独立探测（不落内存 limiter 状态，admin /test 实测用）。
// probeIP 以 "_test" 后缀隔离，避免污染真实客户端的限流计数。
func (rl *RateLimiter) ProbeAllow(ctx context.Context, s scope, probeIP string) bool {
	cfg := rl.snapshotCfg()
	if !cfg.Enabled {
		return true
	}
	// 复用 allow 的判定路径，但封禁/计数写入都带 _test 键
	if rl.isBlocked(ctx, s, probeIP) {
		return false
	}
	var ok bool
	if rdb := rl.snapshotRedis(); cfg.UseRedis && rdb != nil {
		ok = rl.allowRedis(ctx, rdb, s, probeIP)
	} else {
		limiter := rl.getLimiter(s, probeIP)
		ok = limiter == nil || limiter.Allow()
	}
	if !ok {
		rl.addBlock(ctx, s, probeIP)
	}
	return ok
}

// Stats 返回限流器运行状态（/admin/ratelimit/status 用）
func (rl *RateLimiter) Stats() map[string]interface{} {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	active := map[string]int{}
	for s, kl := range rl.limiters {
		active[string(s)] = kl.Len()
	}
	blockedActive := 0
	now := time.Now()
	for _, until := range rl.blocked {
		if until.After(now) {
			blockedActive++
		}
	}
	return map[string]interface{}{
		"enabled":         rl.cfg.Enabled,
		"config":          rl.cfg,
		"active_limiters": active,
		"blocked_current": blockedActive,
		"blocked_total":   rl.blockedTotal,
		"backend":         backendName(rl.cfg.UseRedis, rl.rdb != nil),
	}
}

func backendName(useRedis bool, hasRDB bool) string {
	if useRedis && hasRDB {
		return "redis"
	}
	return "memory"
}

// Middleware 返回 Hertz 限流中间件
//   - scope: 限流维度（global / upload / download / login）
func (rl *RateLimiter) Middleware(s scope) app.HandlerFunc {
	return func(c context.Context, ctx *app.RequestContext) {
		// 快照读（2026-11-06）：热路径不直接读 rl.cfg，见 snapshotCfg。
		if !rl.snapshotCfg().Enabled {
			ctx.Next(c)
			return
		}
		ip := ClientIP(ctx)
		if !rl.allow(c, s, ip) {
			// 门面调用（内部 get() 懒解析），不再手工裸读全局判空
			logger.Warn("[ratelimit] blocked",
				zap.String("scope", string(s)),
				zap.String("ip", ip))
			resp.NewErrorByCode(ctx, errcode.CodeRateLimit)
			ctx.Abort()
			return
		}
		ctx.Next(c)
	}
}

// GlobalMiddleware 全局限流中间件（用于所有接口）
func (rl *RateLimiter) GlobalMiddleware() app.HandlerFunc {
	return rl.Middleware(scopeGlobal)
}

// UploadMiddleware 上传接口限流
func (rl *RateLimiter) UploadMiddleware() app.HandlerFunc {
	return rl.Middleware(scopeUpload)
}

// DownloadMiddleware 下载接口限流
func (rl *RateLimiter) DownloadMiddleware() app.HandlerFunc {
	return rl.Middleware(scopeDownload)
}

// LoginMiddleware 登录接口限流
func (rl *RateLimiter) LoginMiddleware() app.HandlerFunc {
	return rl.Middleware(scopeLogin)
}

// DefaultRateLimiter 全局默认限流器（单例）
//
// ⚠️ **并发安全**（2026-10-31 修）：与 defaultLockout 同款问题——此前是裸包级
// 指针 + 无锁 lazy-init，而登录/上传等限流面在并发下会同时调用
// GetDefaultRateLimiter() ⇒ data race（未定义行为：可能造出两个限流器
// 实例 ⇒ 限流额度被分裂，或拿到半初始化的实例）。
//
// ⇒ 读走 RLock，写（Init/重置）走 Lock；lazy-init 双检。
var (
	defaultLimiterMu sync.RWMutex
	defaultLimiter   *RateLimiter
)

// InitDefaultRateLimiter 初始化默认限流器
func InitDefaultRateLimiter(cfg RateLimitConfig) *RateLimiter {
	rl := NewRateLimiter(cfg)
	defaultLimiterMu.Lock()
	defaultLimiter = rl
	defaultLimiterMu.Unlock()
	return rl
}

// GetDefaultRateLimiter 获取默认限流器
func GetDefaultRateLimiter() *RateLimiter {
	defaultLimiterMu.RLock()
	rl := defaultLimiter
	defaultLimiterMu.RUnlock()
	if rl != nil {
		return rl
	}

	defaultLimiterMu.Lock()
	defer defaultLimiterMu.Unlock()
	if defaultLimiter == nil {
		defaultLimiter = NewRateLimiter(DefaultRateLimitConfig())
	}
	return defaultLimiter
}
