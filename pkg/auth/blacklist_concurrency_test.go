package auth

// JWT 注销黑名单的**并发安全**回归（2026-10-31）。
//
// # 缺陷形态
//
// globalBlacklist 此前是**裸包级指针** + 无锁 lazy-init：
//
//	func getBlacklist() *tokenBlacklist {
//	    if globalBlacklist == nil {          // ← 读
//	        globalBlacklist = &tokenBlacklist{...} // ← 写
//	    }
//	    return globalBlacklist
//	}
//
// # 为什么这条比普通的 lazy-init race 更严重
//
// getBlacklist() 同时被**认证中间件**（每个请求都查 IsTokenRevoked）与
// **注销端点**（每次注销都写 RevokeToken）并发调用。
//
// 并发下造出**两个** tokenBlacklist 实例 ⇒ **注销黑名单分裂**：
// 吊销写进实例 A、后续校验查实例 B ⇒ **已注销的 token 仍然可用**
// ⇒ 「退出登录」在并发下可能**根本没生效**（会话吊销失效）。
//
// 这与 middleware.GetDefaultLockout() 是同款形态（同一批修复）。
//
// # 本文件钉住什么
//
//  1. 并发取单例 ⇒ 必须拿到**同一个**实例（不能分裂）；
//  2. 并发吊销 + 并发查询 ⇒ 不得 data race，且**吊销不得丢失**
//     （吊销写进 A、查询查 B 的分裂形态 ⇒ 查不到 ⇒ 断言失败）；
//  3. **负对照**：把实现退回无锁写法时，用例必须红。

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetBlacklist 归零到"未初始化"状态，让 lazy-init 路径真正被走到。
func resetBlacklist(t *testing.T) {
	t.Helper()
	globalBlacklistMu.Lock()
	globalBlacklist = nil
	globalBlacklistMu.Unlock()
	t.Cleanup(func() { SetBlacklistRedis(nil) })
}

// TestGetBlacklist_ConcurrentSameInstance 并发取黑名单 ⇒ 同一实例。
func TestGetBlacklist_ConcurrentSameInstance(t *testing.T) {
	resetBlacklist(t)

	const workers = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	got := make([]*tokenBlacklist, workers)

	for i := 0; i < workers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got[i] = getBlacklist()
		}()
	}
	close(start)
	wg.Wait()

	first := got[0]
	require.NotNil(t, first)
	for i, bl := range got {
		require.NotNil(t, bl, "worker %d 拿到了 nil 实例", i)
		assert.Same(t, first, bl,
			"并发取黑名单必须拿到**同一个**实例（worker %d 拿到了另一个 ⇒ 无锁 lazy-init 造出了两个实例 ⇒ 注销黑名单分裂）", i)
	}
}

// TestBlacklist_ConcurrentRevokeAndCheck_Concurrent 不得丢吊销。
//
// 这是**行为级**断言：并发吊销 N 个 token 后，逐个查询都必须查到已吊销。
// 若单例分裂（吊销写 A、查 B），这条必然失败 ⇒ 证明缺陷的实际后果。
func TestBlacklist_ConcurrentRevokeAndCheck_Concurrent(t *testing.T) {
	resetBlacklist(t)
	ctx := context.Background()

	const tokens = 64
	revoked := make([]string, tokens)
	for i := range revoked {
		revoked[i] = "token-" + time.Now().Format("150405.000000000") + "-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, tok := range revoked {
		tok := tok
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			RevokeToken(ctx, tok, time.Hour)
		}()
	}
	close(start)
	wg.Wait()

	for _, tok := range revoked {
		assert.True(t, IsTokenRevoked(ctx, tok),
			"并发吊销后 token 必须查得到已吊销（查不到 ⇒ 单例分裂 ⇒ 注销在并发下失效）")
	}
}

// TestSetBlacklistRedis_ConcurrentWithGet 并发注入 + 并发取 ⇒ 不得 race。
func TestSetBlacklistRedis_ConcurrentWithGet(t *testing.T) {
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
				_ = getBlacklist()
			} else {
				SetBlacklistRedis(nil)
			}
		}()
	}
	close(start)
	wg.Wait()

	assert.NotNil(t, getBlacklist(), "并发取用之后黑名单仍须可用")
}
