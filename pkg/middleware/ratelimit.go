// Package middleware 提供 Hertz 中间件：
//   - RateLimit: IP 维度 + 接口维度双层限流
//
// 实现：默认进程内 token bucket（x/time/rate）；配置 rate_limit.use_redis=true
// 且 Redis 可用时切换为 Redis 固定窗口计数（多实例共享）。
// 命中限流后按 block_seconds 封禁该 IP+维度（封禁同样 Redis/内存双写）。
// 配置可经 /admin/ratelimit/config 运行时热更。
package middleware

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"golang.org/x/time/rate"

	"github.com/pigeonbox/contracts/errcode"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/logger"
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

// clientLimiter 单 IP 在某 scope 下的 limiter
type clientLimiter struct {
	limiter    *rate.Limiter
	lastAccess time.Time
}

// RateLimiter 限流管理器
type RateLimiter struct {
	mu      sync.Mutex
	clients map[scope]map[string]*clientLimiter
	blocked map[string]time.Time // "scope|ip" → 封禁截止
	cfg     RateLimitConfig
	rdb     *redis.Client
	stopCh  chan struct{}

	blockedTotal int64 // 累计封禁次数（观测用）
}

// NewRateLimiter 创建限流管理器
func NewRateLimiter(cfg RateLimitConfig) *RateLimiter {
	rl := &RateLimiter{
		clients: map[scope]map[string]*clientLimiter{
			scopeGlobal:   {},
			scopeUpload:   {},
			scopeDownload: {},
			scopeLogin:    {},
		},
		blocked: map[string]time.Time{},
		cfg:     cfg,
		stopCh:  make(chan struct{}),
	}
	// 后台定期清理过期 limiter（防止 map 无限增长）
	go rl.gcLoop()
	return rl
}

// SetRedis 注入 Redis（use_redis=true 时启用分布式固定窗口）
func (rl *RateLimiter) SetRedis(rdb *redis.Client) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.rdb = rdb
}

// UpdateConfig 热更新配置
func (rl *RateLimiter) UpdateConfig(cfg RateLimitConfig) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.cfg = cfg
	// 清空所有现有 limiter，让新配置生效
	for s := range rl.clients {
		rl.clients[s] = map[string]*clientLimiter{}
	}
}

// Stop 停止后台 goroutine
func (rl *RateLimiter) Stop() {
	close(rl.stopCh)
}

func (rl *RateLimiter) gcLoop() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-rl.stopCh:
			return
		case <-t.C:
			rl.gc()
		}
	}
}

func (rl *RateLimiter) gc() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	cutoff := time.Now().Add(-10 * time.Minute)
	for s := range rl.clients {
		for ip, cl := range rl.clients[s] {
			if cl.lastAccess.Before(cutoff) {
				delete(rl.clients[s], ip)
			}
		}
	}
	for k, until := range rl.blocked {
		if until.Before(time.Now()) {
			delete(rl.blocked, k)
		}
	}
}

// getLimiter 获取或创建某 IP 在某 scope 下的 limiter
func (rl *RateLimiter) getLimiter(s scope, ip string) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	m, ok := rl.clients[s]
	if !ok {
		return nil
	}
	cl, ok := m[ip]
	if !ok {
		qps := s.qpsOf(rl.cfg)
		if qps <= 0 {
			qps = 1
		}
		burst := rl.cfg.Burst
		if burst <= 0 {
			burst = qps
		}
		cl = &clientLimiter{
			limiter: rate.NewLimiter(rate.Limit(qps), burst),
		}
		m[ip] = cl
	}
	cl.lastAccess = time.Now()
	return cl.limiter
}

// blockKey 封禁/Redis 计数的组合键
func blockKey(s scope, ip string) string { return string(s) + "|" + ip }

// isBlocked 检查（内存 + Redis 双层）
func (rl *RateLimiter) isBlocked(ctx context.Context, s scope, ip string) bool {
	key := blockKey(s, ip)
	if rl.rdb != nil {
		if n, err := rl.rdb.Exists(ctx, "pb:rl:block:"+key).Result(); err == nil && n > 0 {
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
	secs := rl.cfg.BlockSeconds
	if secs <= 0 {
		return
	}
	key := blockKey(s, ip)
	until := time.Now().Add(time.Duration(secs) * time.Second)
	rl.mu.Lock()
	rl.blocked[key] = until
	rl.blockedTotal++
	rl.mu.Unlock()
	if rl.rdb != nil {
		rl.rdb.Set(ctx, "pb:rl:block:"+key, "1", time.Duration(secs)*time.Second)
	}
}

// allowRedis Redis 固定窗口计数（1s 窗口，INCR + 首次 EXPIRE）
func (rl *RateLimiter) allowRedis(ctx context.Context, s scope, ip string) bool {
	qps := s.qpsOf(rl.cfg)
	if qps <= 0 {
		qps = 1
	}
	key := fmt.Sprintf("pb:rl:cnt:%s:%s:%d", s, ip, time.Now().Unix())
	n, err := rl.rdb.Incr(ctx, key).Result()
	if err != nil {
		// Redis 故障：回退进程内 bucket
		limiter := rl.getLimiter(s, ip)
		return limiter == nil || limiter.Allow()
	}
	if n == 1 {
		rl.rdb.Expire(ctx, key, 2*time.Second)
	}
	return n <= int64(qps)
}

// allow 检查 IP 在某 scope 下是否被允许
func (rl *RateLimiter) allow(ctx context.Context, s scope, ip string) bool {
	if !rl.cfg.Enabled {
		return true
	}
	if rl.isBlocked(ctx, s, ip) {
		return false
	}
	var ok bool
	if rl.cfg.UseRedis && rl.rdb != nil {
		ok = rl.allowRedis(ctx, s, ip)
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
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.cfg
}

// ProbeAllow 独立探测（不落内存 limiter 状态，admin /test 实测用）。
// probeIP 以 "_test" 后缀隔离，避免污染真实客户端的限流计数。
func (rl *RateLimiter) ProbeAllow(ctx context.Context, s scope, probeIP string) bool {
	if !rl.cfg.Enabled {
		return true
	}
	// 复用 allow 的判定路径，但封禁/计数写入都带 _test 键
	if rl.isBlocked(ctx, s, probeIP) {
		return false
	}
	var ok bool
	if rl.cfg.UseRedis && rl.rdb != nil {
		ok = rl.allowRedis(ctx, s, probeIP)
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
	for s, m := range rl.clients {
		active[string(s)] = len(m)
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
		if !rl.cfg.Enabled {
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
var defaultLimiter *RateLimiter

// InitDefaultRateLimiter 初始化默认限流器
func InitDefaultRateLimiter(cfg RateLimitConfig) *RateLimiter {
	defaultLimiter = NewRateLimiter(cfg)
	return defaultLimiter
}

// GetDefaultRateLimiter 获取默认限流器
func GetDefaultRateLimiter() *RateLimiter {
	if defaultLimiter == nil {
		defaultLimiter = NewRateLimiter(DefaultRateLimitConfig())
	}
	return defaultLimiter
}
