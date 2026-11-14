package auth

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSessionExpiryHotUpdateAndIssueConcurrent core#153 正判据：
// 管理台热更写（SetSessionExpiry）与登录/刷新签发读（GenerateTokenWithSession、
// SessionExpiry）并发作用于同一会话时长，-race 下必须零 DATA RACE；
// 同时验证热更语义不丢：写后读到新值、新签发 token 的 exp 按新时长计算。
// 负对照：把 sessionExpiryNanos 改回裸包级 time.Duration 变量 ⇒ 本用例 -race 转红。
func TestSessionExpiryHotUpdateAndIssueConcurrent(t *testing.T) {
	orig := SessionExpiry()
	t.Cleanup(func() { SetSessionExpiry(orig) })

	// 语义前置：热更写后读到新值
	SetSessionExpiry(12 * time.Hour)
	require.Equal(t, 12*time.Hour, SessionExpiry())

	const iterations = 300
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(2)
	// 写侧：模拟管理台反复热更「会话过期时间」（热更调用点 app/config/service.go）
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				SetSessionExpiry(12 * time.Hour)
			} else {
				SetSessionExpiry(168 * time.Hour)
			}
		}
	}()
	// 读侧：登录/刷新签发 + 诊断读，跑完通知写侧收工
	go func() {
		defer wg.Done()
		defer close(stop)
		for i := 0; i < iterations; i++ {
			if _, err := GenerateTokenWithSession(1, "alice", "user", "sid-1"); err != nil {
				t.Errorf("签发第 %d 次失败: %v", i+1, err)
				return
			}
			_ = SessionExpiry()
		}
	}()
	wg.Wait()

	// 收口语义：最终热更值可读、签发口径随之生效
	SetSessionExpiry(168 * time.Hour)
	require.Equal(t, 168*time.Hour, SessionExpiry())
	tok, err := GenerateTokenWithSession(1, "alice", "user", "sid-1")
	require.NoError(t, err)
	claims, err := ParseToken(tok)
	require.NoError(t, err)
	require.NotNil(t, claims.ExpiresAt)
	assert.WithinDuration(t, time.Now().Add(168*time.Hour), claims.ExpiresAt.Time, 5*time.Second,
		"新签发 token 的 exp 必须按热更后的会话时长计算")
}
