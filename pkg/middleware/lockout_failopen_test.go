package middleware

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pigeonbox/core/conf"
	"github.com/redis/go-redis/v9"
)

// ==================== 假 Redis（只为制造「故障」与「已锁定」两种确定状态）====================
//
// 为什么需要假件：CheckLocked 的 fail-open 缺陷**只有 Redis 报错时**才暴露
// （旧实现是 ttl, err := TTL(..); if err == nil && ttl > 0 { 锁定 }; return 0,false
// ⇒ 故障期恒放行）。本机测试里无法稳定制造真实 Redis 故障，而 Lockout.rdb 的
// 字段类型已收窄为方法集接口（redisLockCmds）⇒ 注入假件即可。

var errRedisDown = errors.New("redis: connection refused (fake)")

// deadRedis 所有命令都返回错误 = Redis 全链路故障。
type deadRedis struct{}

func (deadRedis) TTL(context.Context, string) *redis.DurationCmd {
	return redis.NewDurationResult(0, errRedisDown)
}

func (deadRedis) Incr(context.Context, string) *redis.IntCmd {
	return redis.NewIntResult(0, errRedisDown)
}

func (deadRedis) Expire(context.Context, string, time.Duration) *redis.BoolCmd {
	return redis.NewBoolResult(false, errRedisDown)
}

func (deadRedis) Set(context.Context, string, any, time.Duration) *redis.StatusCmd {
	return redis.NewStatusResult("", errRedisDown)
}

func (deadRedis) Del(context.Context, ...string) *redis.IntCmd {
	return redis.NewIntResult(0, errRedisDown)
}

func (deadRedis) IncrWithTTL(context.Context, string, int64, time.Duration) (int64, error) {
	return 0, errRedisDown
}

// lockedRedis 锁定标记已存在（TTL 为正）= Redis 健康且明确「锁着」。
type lockedRedis struct{ ttl time.Duration }

func (f lockedRedis) TTL(context.Context, string) *redis.DurationCmd {
	return redis.NewDurationResult(f.ttl, nil)
}

func (lockedRedis) Incr(context.Context, string) *redis.IntCmd { return redis.NewIntResult(1, nil) }

func (lockedRedis) Expire(context.Context, string, time.Duration) *redis.BoolCmd {
	return redis.NewBoolResult(true, nil)
}

func (lockedRedis) Set(context.Context, string, any, time.Duration) *redis.StatusCmd {
	return redis.NewStatusResult("OK", nil)
}

func (lockedRedis) Del(context.Context, ...string) *redis.IntCmd { return redis.NewIntResult(1, nil) }

func (lockedRedis) IncrWithTTL(context.Context, string, int64, time.Duration) (int64, error) {
	return 1, nil
}

// newTestLockout 用给定 Redis 假件装配锁定器（阈值 3 次 / 窗 300s / 锁 600s）。
func newTestLockout(rdb redisLockCmds) *Lockout {
	cfg := conf.LockoutConfig{
		Enabled:       true,
		MaxAttempts:   3,
		WindowSeconds: 300,
		LockSeconds:   600,
	}
	return &Lockout{rdb: rdb, cfg: cfg, mem: newMemLockout(cfg)}
}

// ==================== 用例 ====================

// TestLockout_RedisDownFailsClosed 是 2026-11-06 fail-open 修复的主判据。
//
// 缺陷形态：Redis 故障时 RecordFailure **回落到内存记账**（这是有意的降级），
// 但 CheckLocked 此前在 Redis 报错时直接 return 0, false
// ⇒ 失败次数照记、锁定判定恒放行 ⇒ 爆破防护在故障期事实上关闭。
//
// 断言：Redis 全程报错 + 失败次数达到阈值 ⇒ CheckLocked 必须报「锁定中」。
func TestLockout_RedisDownFailsClosed(t *testing.T) {
	ctx := context.Background()
	l := newTestLockout(deadRedis{})

	// 正对照：故障期的失败**确实被内存记下来了**（否则本条判据没有意义）
	var locked bool
	for i := 0; i < 3; i++ {
		locked, _ = l.RecordFailure(ctx, "ip:203.0.113.9")
	}
	if !locked {
		t.Fatal("Redis 故障期：失败次数达到阈值，RecordFailure 必须回落到内存并报锁定")
	}

	remain, isLocked := l.CheckLocked(ctx, "ip:203.0.113.9")
	if !isLocked {
		t.Fatal("Redis 故障期：内存里已锁定，CheckLocked 却报放行（fail-open 回归）")
	}
	if remain <= 0 {
		t.Fatalf("锁定剩余秒数应为正，实际 %d", remain)
	}
}

// TestLockout_RedisDownOtherKeyStillAllowed 负对照：故障期不得把「没失败过」的
// 主体一起锁住——降级只能收紧、不能误伤。
func TestLockout_RedisDownOtherKeyStillAllowed(t *testing.T) {
	ctx := context.Background()
	l := newTestLockout(deadRedis{})
	for i := 0; i < 3; i++ {
		l.RecordFailure(ctx, "ip:203.0.113.9")
	}
	if _, isLocked := l.CheckLocked(ctx, "ip:198.51.100.7"); isLocked {
		t.Fatal("另一个从未失败过的 IP 被误锁 ⇒ 降级路径把锁定维度搞错了")
	}
}

// TestLockout_RedisHealthyStillShortCircuits Redis 健康且明确锁定时应直接短路，
// 不依赖内存（多实例共享锁定的语义靠这条）。
func TestLockout_RedisHealthyStillShortCircuits(t *testing.T) {
	ctx := context.Background()
	l := newTestLockout(lockedRedis{ttl: 42 * time.Second})
	remain, isLocked := l.CheckLocked(ctx, "ip:203.0.113.9")
	if !isLocked {
		t.Fatal("Redis 明确给了正 TTL，CheckLocked 却报放行")
	}
	// int(ttl.Seconds())+1 ⇒ 43；只断言量级，避免把取整细节焊死
	if remain < 42 || remain > 44 {
		t.Fatalf("剩余秒数应约等于 43，实际 %d", remain)
	}
}

// TestLockout_MemoryModeUnaffected 纯内存模式（未装配 Redis）行为不变。
func TestLockout_MemoryModeUnaffected(t *testing.T) {
	ctx := context.Background()
	l := newTestLockout(nil)
	var locked bool
	for i := 0; i < 3; i++ {
		locked, _ = l.RecordFailure(ctx, "ip:203.0.113.9")
	}
	if !locked {
		t.Fatal("纯内存模式：未装配 Redis 时失败阈值到了必须锁定")
	}
	if _, isLocked := l.CheckLocked(ctx, "ip:203.0.113.9"); !isLocked {
		t.Fatal("纯内存模式：CheckLocked 未报锁定")
	}
	// 成功后重置 ⇒ 解锁（Reset 必须同时清内存计数）
	l.Reset(ctx, "ip:203.0.113.9")
	if _, isLocked := l.CheckLocked(ctx, "ip:203.0.113.9"); isLocked {
		t.Fatal("Reset 之后仍报锁定（内存计数未清）")
	}
}
