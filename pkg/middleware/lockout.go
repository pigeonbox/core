package middleware

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oekit/concurrency/ratelimit"
	"github.com/pigeonbox/contracts/errcode"
	"github.com/pigeonbox/core/conf"
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
	// IncrWithTTL 计数 + 首次命中才设 TTL（**原子**，收编到 oekit/redisx 的
	// Counter.IncrWithTTL）——手写两步 Incr + if n==1 { Expire } 在两步之间
	// 失败 ⇒ 计数键永不过期 ⇒ 该主体被永久锁定。
	IncrWithTTL(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, error)
}

// memLockoutMax 内存兜底表容量护栏（2026-10-05 审计 P2 引入；现由
// LockoutOf.MaxEntries 托管——表满 fail-open 丢弃失败样本 + OverCapHits
// 观测，不再"整表清空保命"）。
const memLockoutMax = 65536

// Lockout 登录/取件失败计数锁定（防爆破，非 QPS 语义）。
//
// 维度由调用方决定（建议 "ip"、"user:xxx"、"code:xxx" 组合键）：
//   - 窗口内失败次数达到 MaxAttempts → 锁定 LockSeconds
//   - Redis 可用时计数/锁定共享（多实例生效）；否则进程内存兜底
//     （2026-10-18 起兜底托管给 oekit concurrency v0.12.0 ratelimit.Lockout
//     ——concurrency#21 落地物；滑动窗口/锁定条目免空闲回收/容量 fail-open
//     均为其契约能力，替代手搓 memHit/memFail+周期 GC 约 80 行）
type Lockout struct {
	rdb redisLockCmds
	cfg conf.LockoutConfig

	mem *ratelimit.Lockout // 内存兜底（Redis 模式下仅故障降级路径触达）
}

// newMemLockout 内存兜底实例。IdleTTL 取 window+lock：覆盖"失败窗→锁定"完整
// 生命周期（锁定中条目上游免回收；未锁定条目超窗+锁长后无存活语义可回收）。
func newMemLockout(cfg conf.LockoutConfig) *ratelimit.Lockout {
	window := time.Duration(cfg.WindowSeconds) * time.Second
	lock := time.Duration(cfg.LockSeconds) * time.Second
	return ratelimit.NewLockout(ratelimit.LockoutConfig{
		Window:       window,
		Threshold:    cfg.MaxAttempts,
		LockDuration: lock,
		MaxEntries:   memLockoutMax,
		IdleTTL:      window + lock,
	})
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
		// 包一层（纯包装：不新建连接、不改连接参数）+ 补上原子的 IncrWithTTL。
		cmds = newRedisCmds(rdb)
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
	return &Lockout{rdb: cmds, cfg: cfg, mem: newMemLockout(cfg)}
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
	if locked, remain := l.mem.CheckLocked(key); locked {
		return int(remain.Seconds()) + 1, true
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
		// 计数 + 首次命中才设失败窗口：**原子**（收编到 oekit/redisx 的
		// Counter.IncrWithTTL）——此前手写两步，两步之间失败 ⇒ 计数键永不过期
		// ⇒ 失败计数只增不减 ⇒ 该主体被永久锁定。
		count, err := l.rdb.IncrWithTTL(ctx, failKeyPrefix+key, 1, window)
		if err == nil {
			if count >= int64(l.cfg.MaxAttempts) {
				l.rdb.Set(ctx, lockKeyPrefix+key, "1", lock)
				l.rdb.Del(ctx, failKeyPrefix+key)
				return true, l.cfg.LockSeconds
			}
			return false, 0
		}
		// Redis 故障 → 内存兜底
	}

	locked, _ := l.mem.RecordFailure(key)
	if locked {
		return true, l.cfg.LockSeconds
	}
	return false, 0
}

// Reset 成功后清空失败计数。
func (l *Lockout) Reset(ctx context.Context, key string) {
	if l.rdb != nil {
		l.rdb.Del(ctx, failKeyPrefix+key)
	}
	l.mem.Reset(key)
}

// defaultLockout 全局单例（bootstrap 注入 Redis 后创建）
//
// ⚠️ **并发安全**（2026-10-31 修）：此前是**裸包级指针** + 无锁 lazy-init，
// 而 GetDefaultLockout() 被 7 个**安全相关**面调用（setup 门禁 / 登录爆破 /
// 管理后台爆破 / 取件码爆破 / 匿名取件 / preview / API Key 限流）⇒
// 并发请求下「读-判空-写」是经典 data race（Go 内存模型下未定义行为：
// 可能返回两个不同的 Lockout 实例 ⇒ 内存兜底计数分裂，Redis 模式也可能
// 拿到半初始化的实例）。
//
// 症状：`go test -race` 在并发首启用例上直接报 DATA RACE
// （pkg/middleware/lockout.go 的读与写并发）。
//
// ⇒ 读走 RLock，写（Init/重置）走 Lock。lazy-init 用双检 + 写锁。
var (
	defaultLockoutMu sync.RWMutex
	defaultLockout   *Lockout
)

// InitDefaultLockout 初始化全局锁定器（bootstrap 调用一次）
//
// 兼作**测试重置**入口（传 nil 退回内存模式）⇒ 写操作受锁保护。
func InitDefaultLockout(rdb *redis.Client) *Lockout {
	lo := NewLockout(rdb)
	defaultLockoutMu.Lock()
	defaultLockout = lo
	defaultLockoutMu.Unlock()
	return lo
}

// GetDefaultLockout 获取全局锁定器（未初始化时用纯内存模式兜底，保证 handler 可用）
//
// 并发安全：先无锁读一次（快路径，绝大多数调用命中已初始化实例），
// 未初始化时再加写锁做 double-check 初始化。
func GetDefaultLockout() *Lockout {
	defaultLockoutMu.RLock()
	lo := defaultLockout
	defaultLockoutMu.RUnlock()
	if lo != nil {
		return lo
	}

	defaultLockoutMu.Lock()
	defer defaultLockoutMu.Unlock()
	// double-check：等锁期间可能已被别的 goroutine 初始化
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
