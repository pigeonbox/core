package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/oekit/pkg/cache"
	"github.com/redis/go-redis/v9"
)

// redisBlacklistCmds 注销黑名单所需的最小 Redis 命令集（*redis.Client 天然满足；
// 字段收窄为方法集以便注入内存 mock，注入函数仍收具体客户端）。
type redisBlacklistCmds interface {
	Set(ctx context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd
	Exists(ctx context.Context, keys ...string) *redis.IntCmd
}

// tokenBlacklist JWT 注销黑名单（logout 端点写入，认证中间件查询）。
// 键 = token 的 SHA-256（避免明文 token 进入存储），TTL = token 剩余有效期。
// Redis 可用时多实例共享；否则进程内存兜底（单实例部署语义不变）。
type tokenBlacklist struct {
	rdb redisBlacklistCmds

	// mem 进程内存兜底表（Redis 故障降级窗口用）：oekit/pkg/cache.LocalCache
	// （逐条 TTL = token 剩余有效期，容量上限 + 过期回收托管），替代此前手搓
	// 「map[until] + 惰性 GC」约 40 行。
	//
	// ⚠️ 淘汰策略差异（相对手搓版）：超容时手搓版丢「最早过期」条目（保护损失
	// 最小），LocalCache 的 EvictFIFO 丢「最早写入」——仅在「Redis 故障期间
	// 单实例累积 > memBlacklistMax 条未过期吊销」这一双重故障场景才可见，
	// 且被丢条目的 token 仍在自身 TTL 内自然过期；换来的收益是内存有界与
	// 过期清扫不再自理。若未来要恢复「最早过期先丢」语义，换 EvictPolicy
	// 不够（那是按写/读序），需要在上游 oekit/pkg/cache 加淘汰策略。
	mem *cache.LocalCache[struct{}]
}

// newMemBlacklist 构造内存兜底表。
func newMemBlacklist() *cache.LocalCache[struct{}] {
	return cache.NewLocalCache(cache.LocalConfig[struct{}]{
		MaxSize: memBlacklistMax,
		NullTTL: -1, // 不写 null marker：本表只用 Set/GetEx，无回源
	})
}

// 供 auth 包内部单例使用
//
// ⚠️ **并发安全**（2026-10-31 修）：此前是**裸包级指针** + 无锁 lazy-init，
// 而 getBlacklist() 被**认证中间件**（每个请求都查）与**注销端点**（每个注销
// 都写）并发调用 ⇒ 经典 data race（Go 内存模型下未定义行为）：
// 可能造出**两个** tokenBlacklist 实例 ⇒ **JWT 注销黑名单分裂**
// ⇒ 吊销写进实例 A、校验查实例 B ⇒ **已注销的 token 仍然可用**（会话注销失效）。
//
// 同款形态的 middleware.GetDefaultLockout() 已在同批修复（见 pkg/middleware）。
//
// ⇒ 读走 RLock，写（SetBlacklistRedis / 测试重置）走 Lock；lazy-init 双检。
var (
	globalBlacklistMu sync.RWMutex
	globalBlacklist   *tokenBlacklist
)

// SetBlacklistRedis 注入 Redis（bootstrap 调用；nil = 纯内存模式）。
// nil 归一化：typed-nil 接口会骗过内存模式守卫（!= nil 判真）。
func SetBlacklistRedis(rdb *redis.Client) {
	var cmds redisBlacklistCmds
	if rdb != nil {
		cmds = rdb
	}
	bl := &tokenBlacklist{rdb: cmds, mem: newMemBlacklist()}
	globalBlacklistMu.Lock()
	old := globalBlacklist
	globalBlacklist = bl
	globalBlacklistMu.Unlock()
	if old != nil {
		// 换实例后回收旧表的清扫协程（Close 只停清扫，不影响在用读者）
		_ = old.mem.Close(context.Background())
	}
}

func getBlacklist() *tokenBlacklist {
	globalBlacklistMu.RLock()
	bl := globalBlacklist
	globalBlacklistMu.RUnlock()
	if bl != nil {
		return bl
	}

	globalBlacklistMu.Lock()
	defer globalBlacklistMu.Unlock()
	// double-check：等锁期间可能已被别的 goroutine 初始化
	if globalBlacklist == nil {
		globalBlacklist = &tokenBlacklist{mem: newMemBlacklist()}
	}
	return globalBlacklist
}

func tokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "pb:jwt:bl:" + hex.EncodeToString(sum[:])
}

// memBlacklistMax 内存兜底表容量上限（防无界增长；只影响降级窗口内写入量）
const memBlacklistMax = 65536

// RevokeToken 注销 token（至其自然过期前一直拦截）。
// Redis 故障时降级写内存兜底（2026-10-05 审计 P2：此前 Set 错误被吞、查询
// fail-open，Redis 抖动窗口内已注销 token 静默恢复可用）。
func RevokeToken(ctx context.Context, token string, remaining time.Duration) {
	if token == "" || remaining <= 0 {
		return
	}
	bl := getBlacklist()
	if bl.rdb != nil {
		if err := bl.rdb.Set(ctx, tokenKey(token), "1", remaining).Err(); err == nil {
			return
		}
		// Redis 写失败 → 落内存，吊销不因基础设施抖动丢失
	}
	bl.memRevoke(ctx, tokenKey(token), remaining)
}

// IsTokenRevoked 查询 token 是否已注销。
// Redis 查询失败时回退内存兜底（与 RevokeToken 的降级写配对，故障窗口内
// 写入内存的吊销仍可被查到）。
func IsTokenRevoked(ctx context.Context, token string) bool {
	if token == "" {
		return false
	}
	bl := getBlacklist()
	if bl.rdb != nil {
		n, err := bl.rdb.Exists(ctx, tokenKey(token)).Result()
		if err == nil {
			return n > 0
		}
	}
	return bl.memRevoked(ctx, tokenKey(token))
}

// memRevoke 写内存兜底（逐条 TTL = token 剩余有效期）。
func (bl *tokenBlacklist) memRevoke(ctx context.Context, key string, remaining time.Duration) {
	_ = bl.mem.SetWithTTL(ctx, key, &struct{}{}, remaining)
}

func (bl *tokenBlacklist) memRevoked(ctx context.Context, key string) bool {
	_, ok, _ := bl.mem.GetEx(ctx, key)
	return ok
}
