package middleware

// Redis 计数原语的收编（2026-10-27）。
//
// ## 为什么收编
//
// 「计数 + 首次命中才设 TTL」此前是**手写的两步**：
//
//	n, err := rdb.Incr(ctx, key).Result()
//	if n == 1 { rdb.Expire(ctx, key, window) }
//
// 两步之间失败（网络抖动）⇒ 计数键**永不过期** ⇒ 限流/失败锁定的计数只增不减
// ⇒ 该 IP/账号被**永久封禁**（直到 Redis 内存淘汰）。
//
// 收编到 **oekit/redisx** 的 `Counter.IncrWithTTL`（Lua 原子）：
// 「已有 TTL 只增加不影响原 TTL；无 TTL 才设置」⇒ 与手写版**逐条语义一致**，
// 但没有中间态 ⇒ 上述永久封禁的失败模式消失。
//
// ## 为什么用 NewClientFromClient 包住既有客户端
//
// `redisx.NewClient` 会**改写**调用方未设置的连接默认参数（DialTimeout /
// ReadTimeout / WriteTimeout / PoolTimeout / MinIdleConns）⇒ 连接语义会变；
// `NewClientFromClient` 是**纯包装**：不新建连接、不动任何连接参数。

import (
	"context"
	"errors"
	"time"

	"github.com/oekit/redisx"
	"github.com/redis/go-redis/v9"
)

// errNoRedisClient 未装配 Redis 时使用（调用方应先判 nil）。
var errNoRedisClient = errors.New("redis 客户端未装配")

// redisCmds 是「Redis 命令集」适配器：包住 go-redis 客户端，并补上**原子**的
// IncrWithTTL（redisx.Counter）。其余命令直接透传。
type redisCmds struct {
	*redis.Client
	counter *redisx.Client
}

// newRedisCmds 包装既有客户端（不新建连接、不改连接参数）。
// ⚠️ 传入 nil 时返回 nil（调用方靠 nil 判空回落内存模式，见 NewLockout）。
func newRedisCmds(c *redis.Client) *redisCmds {
	if c == nil {
		return nil
	}
	return &redisCmds{Client: c, counter: redisx.NewClientFromClient(c)}
}

// IncrWithTTL 计数 + 首次命中才设 TTL（原子）。ttl<=0 时只计数不设 TTL。
func (c *redisCmds) IncrWithTTL(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, error) {
	if c == nil || c.Client == nil {
		return 0, errNoRedisClient
	}
	if ttl <= 0 {
		return c.IncrBy(ctx, key, delta).Result()
	}
	return c.counter.NewCounter(key).IncrWithTTL(ctx, delta, ttl)
}
