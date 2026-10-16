package middleware

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pigeonbox/contracts/errcode"
	"github.com/pigeonbox/core/conf"
	"github.com/oekit/concurrency/async"
	"github.com/redis/go-redis/v9"
)

// redisLockCmds 失败锁定所需的最小 Redis 命令集（*redis.Client 天然满足；
// 字段收窄为方法集以便注入内存 mock，构造函数仍收具体客户端）。
type redisLockCmds interface {
	TTL(ctx context.Context, key string) *redis.DurationCmd
	Incr(ctx context.Context, key string) *redis.IntCmd
	Expire(ctx context.Context, key string, expiration time.Duration) *redis.BoolCmd
	Set(ctx context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
}

// 内存兜底表的容量上限与 GC 周期（2026-10-05 审计 P2：此前 memFail/memHit
// 只在同 key 重访时惰性清理，攻击者用海量唯一键（用户名/取件码）打失败计数
// 可让两表无界增长——内存型 DoS。现周期清扫 + 满载淘汰）。
const (
	memLockoutMax      = 65536
	memLockoutGCPeriod = 10 * time.Minute
)

// Lockout 登录/取件失败计数锁定（防爆破，非 QPS 语义）。
//
// 维度由调用方决定（建议 "ip"、"user:xxx"、"code:xxx" 组合键）：
//   - 窗口内失败次数达到 MaxAttempts → 锁定 LockSeconds
//   - Redis 可用时计数/锁定共享（多实例生效）；否则进程内存兜底
type Lockout struct {
	rdb redisLockCmds
	cfg conf.LockoutConfig

	mu      sync.Mutex
	memHit  map[string]time.Time // key → 锁定截止时间
	memFail map[string]*failWindow

	gcOnce sync.Once
}

type failWindow struct {
	count     int
	windowEnd time.Time
}

const (
	lockKeyPrefix = "pb:lock:hit:"   // 锁定标记
	failKeyPrefix = "pb:lock:count:" // 失败计数
)

// NewLockout 创建失败锁定器。rdb 为 nil 或配置未启用时仍返回可用实例（内存模式）。
// nil 归一化：*redis.Client 的 typed-nil 赋给接口字段后 != nil 判真，会骗过
// 内存模式守卫并在方法调用时 panic——必须在注入边界处理。
func NewLockout(rdb *redis.Client) *Lockout {
	var cmds redisLockCmds
	if rdb != nil {
		cmds = rdb
	}
	cfg := conf.LockoutConfig{
		Enabled:       true,
		MaxAttempts:   10,
		WindowSeconds: 300,
		LockSeconds:   600,
	}
	if c := conf.GetGlobalConfig(); c != nil {
		if c.Security.Lockout.MaxAttempts > 0 {
			cfg = c.Security.Lockout
		}
	}
	l := &Lockout{
		rdb:     cmds,
		cfg:     cfg,
		memHit:  map[string]time.Time{},
		memFail: map[string]*failWindow{},
	}
	return l.withGC()
}

// withGC 启动内存表周期清扫（Redis 模式下表恒空，空转开销可忽略）。
// 常驻协程无监督者，GoSafe 兜底防 gcSweep 未来演进引入 panic 带崩进程。
func (l *Lockout) withGC() *Lockout {
	l.gcOnce.Do(func() {
		async.GoSafe(func() {
			ticker := time.NewTicker(memLockoutGCPeriod)
			defer ticker.Stop()
			for range ticker.C {
				l.gcSweep()
			}
		})
	})
	return l
}

// gcSweep 清扫过期条目；满载仍超限时整表清空（保命优先于保留计数，
// 满载 64K 意味着已被海量唯一键刷过，重置不放大爆破面——限流层仍在）。
func (l *Lockout) gcSweep() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	for k, until := range l.memHit {
		if now.After(until) {
			delete(l.memHit, k)
		}
	}
	for k, w := range l.memFail {
		if now.After(w.windowEnd) {
			delete(l.memFail, k)
		}
	}
	if len(l.memFail) >= memLockoutMax {
		l.memFail = map[string]*failWindow{}
	}
	if len(l.memHit) >= memLockoutMax {
		l.memHit = map[string]time.Time{}
	}
}

// Enabled 是否启用
func (l *Lockout) Enabled() bool { return l.cfg.Enabled }

// CheckLocked 返回 (剩余锁定秒数, 是否被锁)。未启用恒返回 (0,false)。
func (l *Lockout) CheckLocked(ctx context.Context, key string) (int, bool) {
	if !l.cfg.Enabled {
		return 0, false
	}
	if l.rdb != nil {
		ttl, err := l.rdb.TTL(ctx, lockKeyPrefix+key).Result()
		if err == nil && ttl > 0 {
			return int(ttl.Seconds()) + 1, true
		}
		return 0, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if until, ok := l.memHit[key]; ok {
		if remain := time.Until(until); remain > 0 {
			return int(remain.Seconds()) + 1, true
		}
		delete(l.memHit, key)
	}
	return 0, false
}

// RecordFailure 记录一次失败。返回 (是否触发锁定, 锁定秒数)。
func (l *Lockout) RecordFailure(ctx context.Context, key string) (bool, int) {
	if !l.cfg.Enabled {
		return false, 0
	}
	window := time.Duration(l.cfg.WindowSeconds) * time.Second
	lock := time.Duration(l.cfg.LockSeconds) * time.Second

	if l.rdb != nil {
		count, err := l.rdb.Incr(ctx, failKeyPrefix+key).Result()
		if err == nil {
			if count == 1 {
				l.rdb.Expire(ctx, failKeyPrefix+key, window)
			}
			if count >= int64(l.cfg.MaxAttempts) {
				l.rdb.Set(ctx, lockKeyPrefix+key, "1", lock)
				l.rdb.Del(ctx, failKeyPrefix+key)
				return true, l.cfg.LockSeconds
			}
			return false, 0
		}
		// Redis 故障 → 内存兜底
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	// 满载守卫：新键进入前先扫过期，仍满则整表重置（防海量唯一键撑表，
	// 限流层仍在线，此处只兜内存）
	if _, exists := l.memFail[key]; !exists && len(l.memFail) >= memLockoutMax {
		for k, w := range l.memFail {
			if now.After(w.windowEnd) {
				delete(l.memFail, k)
			}
		}
		if len(l.memFail) >= memLockoutMax {
			l.memFail = map[string]*failWindow{}
		}
	}
	w, ok := l.memFail[key]
	if !ok || now.After(w.windowEnd) {
		w = &failWindow{count: 0, windowEnd: now.Add(window)}
		l.memFail[key] = w
	}
	w.count++
	if w.count >= l.cfg.MaxAttempts {
		l.memHit[key] = now.Add(lock)
		delete(l.memFail, key)
		return true, l.cfg.LockSeconds
	}
	return false, 0
}

// Reset 成功后清空失败计数。
func (l *Lockout) Reset(ctx context.Context, key string) {
	if l.rdb != nil {
		l.rdb.Del(ctx, failKeyPrefix+key)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.memFail, key)
}

// defaultLockout 全局单例（bootstrap 注入 Redis 后创建）
var defaultLockout *Lockout

// InitDefaultLockout 初始化全局锁定器（bootstrap 调用一次）
func InitDefaultLockout(rdb *redis.Client) *Lockout {
	defaultLockout = NewLockout(rdb)
	return defaultLockout
}

// GetDefaultLockout 获取全局锁定器（未初始化时用纯内存模式兜底，保证 handler 可用）
func GetDefaultLockout() *Lockout {
	if defaultLockout == nil {
		defaultLockout = NewLockout(nil)
	}
	return defaultLockout
}

// LockedError 被锁定错误（携带剩余秒数）
type LockedError struct {
	RemainingSeconds int
}

func (e *LockedError) Error() string {
	return fmt.Sprintf("尝试过于频繁，已临时锁定，请 %d 秒后重试", e.RemainingSeconds)
}

// ErrCode 转换为统一错误码
func (e *LockedError) ErrCode() int { return errcode.CodeTooManyAttempts }

// LookupLockKey 取件/分享码查询的失败锁定键：查询折叠开启（download.
// code_case_insensitive，默认开）时把 code 归一为大写，使 abc/ABC 等大小写
// 变体共享同一失败计数，防变体绕过 IP+code 维度爆破锁定。
func LookupLockKey(scope, ip, code string) string {
	if conf.CodeFoldEnabledOrDefault() {
		code = strings.ToUpper(code)
	}
	return FormatLockKey(scope, ip, code)
}

// FormatLockKey 组合锁定维度键（值做转义避免键冲突）
func FormatLockKey(parts ...string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "|"
		}
		out += strconv.Quote(p)
	}
	return out
}
