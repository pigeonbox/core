// Package memkv 进程内字符串 TTL KV（零第三方依赖，kit/async 同为纯标准库实现）：单机内存模式的共享存储原语。
//
// 服务对象：Redis 消费方（anonymous/presign 等持窄接口的域）在 redis.host 为空
// 时的等价兜底——取件码映射、直传会话这类短 TTL 数据。语义对齐 Redis 最小子集：
// Get / Set / SetNX（原子）/ Del + TTL 过期；不做持久化、不跨进程。
//
// 防御对齐 lockout 内存兜底先例（2026-10-05 审计 P2）：容量上限 + 周期清扫 +
// 满载守卫——仅靠惰性清理时，攻击者用海量唯一键（取件码枚举/负缓存标记）可把
// 表撑到无界。
//
// 消费方 ≥3 处后可评估上收 kit（当前 anonymous/presign 两个适配层复用本包）。
package memkv

import (
	"sync"
	"time"

	"github.com/pigeonbox/kit/async"
)

const (
	// DefaultMax 单表容量上限。满载先清扫过期，仍满则整表重置（保命优先于
	// 保留数据：满载意味着已被海量唯一键刷过，重置的代价是缓存/标记失效，
	// 重回源路径兜底，不放大业务影响）。
	DefaultMax = 65536
	// DefaultGCPeriod 周期清扫间隔。
	DefaultGCPeriod = 10 * time.Minute
)

type entry struct {
	val string
	exp time.Time // 零值 = 永不过期（调用方恒带 TTL，保留通用性）
}

func (e entry) expired(now time.Time) bool {
	return !e.exp.IsZero() && now.After(e.exp)
}

// Store 字符串 TTL KV。并发安全；清扫协程随首次写入惰性启动（只读不启）。
type Store struct {
	mu       sync.Mutex
	m        map[string]entry
	max      int
	gcPeriod time.Duration
	once     sync.Once
	now      func() time.Time // 可注入时钟（测试）
}

// New 创建 Store（默认容量与清扫周期）。
func New() *Store {
	return &Store{
		m:        map[string]entry{},
		max:      DefaultMax,
		gcPeriod: DefaultGCPeriod,
		now:      time.Now,
	}
}

// WithClock 注入时钟（测试用）。
func (s *Store) WithClock(now func() time.Time) *Store {
	s.now = now
	return s
}

// Len 返回当前条目数（含未过期；测试用）。
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// Get 返回 (值, 是否命中)。过期条目惰性删除。
func (s *Store) Get(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[key]
	if !ok {
		return "", false
	}
	if e.expired(s.now()) {
		delete(s.m, key)
		return "", false
	}
	return e.val, true
}

// Set 写入（覆盖语义，同 Redis SET）。
func (s *Store) Set(key, val string, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.insert(key, entry{val: val, exp: s.expAt(ttl)})
}

// SetNX 不存在才写入，返回是否写入成功（原子，同 Redis SET NX）。
// 已有过期条目视为不存在（写入前失效）。
func (s *Store) SetNX(key, val string, ttl time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.m[key]; ok && !e.expired(s.now()) {
		return false
	}
	s.insert(key, entry{val: val, exp: s.expAt(ttl)})
	return true
}

// Del 删除若干键，返回实际删除数（同 Redis DEL）。
func (s *Store) Del(keys ...string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, k := range keys {
		if _, ok := s.m[k]; ok {
			delete(s.m, k)
			n++
		}
	}
	return n
}

// expAt 由 TTL 计算过期时点（ttl<=0 = 永不过期，对齐 Redis 无过期语义）。
func (s *Store) expAt(ttl time.Duration) time.Time {
	if ttl <= 0 {
		return time.Time{}
	}
	return s.now().Add(ttl)
}

// insert 满载守卫下的写入。调用方须已持锁。
func (s *Store) insert(key string, e entry) {
	s.startGC()
	if _, exists := s.m[key]; !exists && len(s.m) >= s.max {
		s.sweepLocked()
		if len(s.m) >= s.max {
			s.m = map[string]entry{}
		}
	}
	s.m[key] = e
}

// startGC 惰性启动清扫协程（守护协程，随进程存活——与 lockout 先例一致，
// Store 为进程级单例量级，不为短生命周期实例设计回收）。常驻无监督，GoSafe
// 兜底与 lockout 同判。
func (s *Store) startGC() {
	s.once.Do(func() {
		async.GoSafe(func() {
			ticker := time.NewTicker(s.gcPeriod)
			defer ticker.Stop()
			for range ticker.C {
				s.sweepLocked()
			}
		})
	})
}

// sweepLocked 清扫过期条目。调用方须已持锁。
func (s *Store) sweepLocked() {
	now := s.now()
	for k, e := range s.m {
		if e.expired(now) {
			delete(s.m, k)
		}
	}
}
