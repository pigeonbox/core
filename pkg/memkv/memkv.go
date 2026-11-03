// Package memkv 进程内字符串 TTL KV（主包仅依赖标准库与 oekit/pkg/cache）：单机内存模式的共享存储原语。
//
// 服务对象：Redis 消费方（anonymous/presign 等持窄接口的域）在 redis.host 为空
// 时的等价兜底——取件码映射、直传会话这类短 TTL 数据。语义对齐 Redis 最小子集：
// Get / Set / SetNX（原子）/ Del + TTL 过期；不做持久化、不跨进程。
//
// # 收编 oekit/pkg/cache.LocalCache（2026-11-03）
//
// 手搓 map+mutex+TTL 表退役，本包只保留「Redis 最小子集 API 形状」的薄门面
// （字符串键值、无 ctx）。收编顺带根治一个数据竞争：旧实现的周期清扫协程
// （startGC）裸调 sweepLocked（其文档要求「调用方须已持锁」）⇒ 与读写路径的
// s.mu 并发访问同一张 map（-race 必红，线上只因 10min 清扫窗口罕见才未炸）。
//
// 与旧实现的有意差异（满载语义，对齐 lockout 内存兜底先例的「差异记录在案」
// 纪律）：
//   - 旧：满载先清扫过期、仍满则**整表重置**（保命优先于保留数据）；
//   - 新：LocalCache MaxSize=DefaultMax + FIFO 逐出（写满时淘汰最早写入）。
//
// 防 DoS 的内存上界不变（DefaultMax）；差异只在「被撑爆时的附带伤害面」——
// 整表重置殃及全部键，FIFO 只逐出最旧的键。两者同属「缓存/标记失效、回源
// 兜底」的代价类别（DB 仍是唯一真相源），FIFO 严格更温和。
//
// 另两处口径差（均已核消费面无感）：
//   - 过期回收：旧=10min 周期清扫+读路径惰性删除；新=读路径惰性删除（含
//     SetNX 的「过期按不存在」）+满载 FIFO 逐出，无后台清扫 goroutine
//     （NullTTL 显式禁用 ⇒ sweepNeeded=false，进程内零常驻协程）。过期
//     尸体最多驻留到被读到或被逐出，内存上界不变。
//   - Del 计数：旧=map 存在性（过期未回收条目也计数）；新=删前逻辑存在性
//     统计（过期即不存在）。单线程语义一致；与并发写入交错时计数为近似
//     ——消费面无人读计数（redisshim 消费点全走 .Err()，已核）。
//
// 消费方 ≥3 处后可评估上收 kit（当前 anonymous/presign 两个适配层复用本包）。
package memkv

import (
	"context"
	"time"

	"github.com/oekit/pkg/cache"
)

const (
	// DefaultMax 单表容量上限（LocalCache MaxSize：写满 FIFO 逐出，语义见
	// 包文档「满载语义」）。攻击面同旧口径：海量唯一键刷表时内存有界。
	DefaultMax = 65536
	// DefaultGCPeriod 历史自管清扫周期。现无自管清扫（过期回收口径见包
	// 文档），保留常量兼容既有引用。
	DefaultGCPeriod = 10 * time.Minute
)

// Store 字符串 TTL KV。并发安全；零常驻 goroutine（无后台清扫件）。
type Store struct {
	c *cache.LocalCache[string]
}

// New 创建 Store（默认容量；时钟 time.Now）。
func New() *Store {
	return newWithClock(time.Now)
}

// WithClock 注入时钟（测试用）。LocalCache 的时钟在构造时确定，故返回注入
// 时钟的**新实例**；仅测试链式 New().WithClock(...) 形态使用（已核全部调用
// 点），写入后换时钟会丢数据。
func (s *Store) WithClock(now func() time.Time) *Store {
	return newWithClock(now)
}

func newWithClock(now func() time.Time) *Store {
	return &Store{c: cache.NewLocalCache(cache.LocalConfig[string]{
		TTL:     0, // 无默认 TTL：逐操作显式传参，ttl<=0 = 永不过期（同旧 expAt）
		MaxSize: DefaultMax,
		Now:     now,
		NullTTL: -1, // 显式禁用空值缓存：本 KV 无 GetOrLoad/markers 概念；顺带 sweepNeeded=false ⇒ 零后台 goroutine
	})}
}

// Len 返回当前条目数（含未过期；测试用）。
func (s *Store) Len() int { return s.c.Size() }

// Get 返回 (值, 是否命中)。过期条目惰性删除（LocalCache 读路径回收）。
func (s *Store) Get(key string) (string, bool) {
	v, hit, err := s.c.GetEx(context.Background(), key)
	if err != nil || !hit || v == nil {
		return "", false
	}
	return *v, true
}

// Set 写入（覆盖语义，同 Redis SET）。ttl<=0 = 永不过期。
func (s *Store) Set(key, val string, ttl time.Duration) {
	_ = s.c.SetWithTTL(context.Background(), key, &val, ttl)
}

// SetNX 不存在才写入，返回是否写入成功（原子，同 Redis SET NX）。
// 已有过期条目视为不存在（LocalCache.SetNX 契约同口径）。
func (s *Store) SetNX(key, val string, ttl time.Duration) bool {
	ok, _ := s.c.SetNX(context.Background(), key, &val, ttl)
	return ok
}

// Del 删除若干键，返回实际删除数（同 Redis DEL）。计数口径见包文档。
func (s *Store) Del(keys ...string) int {
	ctx := context.Background()
	n := 0
	present := make([]string, 0, len(keys))
	for _, k := range keys {
		if ok, _ := s.c.Exists(ctx, k); ok {
			n++
			present = append(present, k)
		}
	}
	_ = s.c.DelBatch(ctx, present...)
	return n
}
