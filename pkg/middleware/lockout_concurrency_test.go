package middleware

// 全局单例的**并发安全**回归（2026-10-31）。
//
// # 缺陷形态
//
// defaultLockout / defaultLimiter 此前是**裸包级指针** + 无锁 lazy-init：
//
//	func GetDefaultLockout() *Lockout {
//	    if defaultLockout == nil {      // ← 读
//	        defaultLockout = NewLockout(nil) // ← 写
//	    }
//	    return defaultLockout
//	}
//
// 并发调用下「读-判空-写」是经典 data race（Go 内存模型下未定义行为）：
// 可能造出**两个**不同的 Lockout 实例 ⇒ 内存兜底计数分裂（限流/爆破防护
// 被绕过）；也可能让一个 goroutine 拿到**半初始化**的实例。
//
// # 影响面
//
// GetDefaultLockout() 被 **7 个安全相关面**调用：setup 门禁、登录爆破、
// 管理后台爆破、取件码爆破、匿名取件、preview、API Key 限流。
// GetDefaultRateLimiter() 用于登录/上传等限流面。
//
// ⇒ 这不是"理论上的 race"，而是**安全护栏自身的数据竞争**。
//
// # 本文件钉住什么
//
//  1. 并发取全局单例 ⇒ 必须拿到**同一个**实例（不能分裂）；
//  2. 并发取 + 并发 Init（重置）⇒ 不得 data race；
//  3. **负对照**：把实现退回无锁写法时，本文件的用例必须红（go test -race）。
//
// ⚠️ 这些用例在**无锁**实现下未必每次都红（race detector 需要真的撞上），
// 但 §3 的「同一实例」断言是**恒真则必红**的——无锁实现下若两个 goroutine
// 同时判空，就会造出两个实例 ⇒ 断言失败。

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetDefaultLockout_ConcurrentSameInstance 并发取全局锁定器 ⇒ 同一实例。
func TestGetDefaultLockout_ConcurrentSameInstance(t *testing.T) {
	// 归零到"未初始化"状态，让 lazy-init 路径真正被走到
	InitDefaultLockout(nil)
	defaultLockoutMu.Lock()
	defaultLockout = nil
	defaultLockoutMu.Unlock()
	t.Cleanup(func() { InitDefaultLockout(nil) })

	const workers = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	got := make([]*Lockout, workers)

	for i := 0; i < workers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got[i] = GetDefaultLockout()
		}()
	}
	close(start)
	wg.Wait()

	first := got[0]
	require.NotNil(t, first)
	for i, l := range got {
		require.NotNil(t, l, "worker %d 拿到了 nil 实例", i)
		assert.Same(t, first, l,
			"并发取全局锁定器必须拿到**同一个**实例（worker %d 拿到了另一个 ⇒ 无锁 lazy-init 造出了两个实例）", i)
	}
}

// TestGetDefaultLockout_ConcurrentWithInit 并发取 + 并发重置 ⇒ 不得 race。
//
// 这一条是 **-race 下的守门**：退回无锁实现时，go test -race 会直接报
// DATA RACE（读 defaultLockout 与写 defaultLockout 并发）。
func TestGetDefaultLockout_ConcurrentWithInit(t *testing.T) {
	const workers = 16
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				_ = GetDefaultLockout()
			} else {
				InitDefaultLockout(nil)
			}
		}()
	}
	close(start)
	wg.Wait()

	assert.NotNil(t, GetDefaultLockout(), "并发取用之后全局锁定器仍须可用")
}

// TestGetDefaultRateLimiter_ConcurrentSameInstance 并发取全局限流器 ⇒ 同一实例。
func TestGetDefaultRateLimiter_ConcurrentSameInstance(t *testing.T) {
	defaultLimiterMu.Lock()
	defaultLimiter = nil
	defaultLimiterMu.Unlock()
	t.Cleanup(func() { InitDefaultRateLimiter(DefaultRateLimitConfig()) })

	const workers = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	got := make([]*RateLimiter, workers)

	for i := 0; i < workers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got[i] = GetDefaultRateLimiter()
		}()
	}
	close(start)
	wg.Wait()

	first := got[0]
	require.NotNil(t, first)
	for i, rl := range got {
		require.NotNil(t, rl, "worker %d 拿到了 nil 实例", i)
		assert.Same(t, first, rl,
			"并发取全局限流器必须拿到**同一个**实例（worker %d 拿到了另一个 ⇒ 无锁 lazy-init 造出了两个实例）", i)
	}
}

// TestGetDefaultRateLimiter_ConcurrentWithInit 并发取 + 并发重置 ⇒ 不得 race。
func TestGetDefaultRateLimiter_ConcurrentWithInit(t *testing.T) {
	const workers = 16
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				_ = GetDefaultRateLimiter()
			} else {
				InitDefaultRateLimiter(DefaultRateLimitConfig())
			}
		}()
	}
	close(start)
	wg.Wait()

	assert.NotNil(t, GetDefaultRateLimiter(), "并发取用之后全局限流器仍须可用")
}
