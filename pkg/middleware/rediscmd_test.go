package middleware

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIncrWithTTLSemantics 原子计数原语的语义锁（收编到 oekit/redisx 后）。
//
// 语义（与此前手写「Incr + if n==1 { Expire }」**逐条一致**，但没有中间态）：
//
//	· 首次命中：计数 +1，并设 TTL；
//	· 后续命中：只计数，**TTL 不续期**（固定窗口，不是滑动窗口）。
//
// 这条「不续期」是收编的**关键等价性**：若误改成每次续期，
// 持续流量下的计数键永不过期 ⇒ 限流窗口永不结束 ⇒ 限流形同虚设。
func TestIncrWithTTLSemantics(t *testing.T) {
	s := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer func() { _ = client.Close() }()

	cmds := newRedisCmds(client)
	require.NotNil(t, cmds)
	ctx := context.Background()

	// 首次命中：计数 1 + 设 TTL
	n, err := cmds.IncrWithTTL(ctx, "pb:rl:cnt:x", 1, 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	first := s.TTL("pb:rl:cnt:x")
	assert.Greater(t, first, time.Duration(0), "首次命中必须设 TTL")
	assert.LessOrEqual(t, first, 5*time.Second)

	// 第二次命中：计数 2，TTL **不续期**（≤ 首次的值）
	n, err = cmds.IncrWithTTL(ctx, "pb:rl:cnt:x", 1, 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
	second := s.TTL("pb:rl:cnt:x")
	assert.LessOrEqual(t, second, first, "TTL 不得被续期（固定窗口语义）")

	// 时钟推进 3s 后再计数：TTL 继续走（不是重置）
	s.FastForward(3 * time.Second)
	n, err = cmds.IncrWithTTL(ctx, "pb:rl:cnt:x", 1, 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, int64(3), n)
	assert.LessOrEqual(t, s.TTL("pb:rl:cnt:x"), first-3*time.Second+time.Second,
		"TTL 必须随时间流逝而不是被重置")
}

// TestNewRedisCmdsNil 客户端缺席时不得 panic（调用方靠 nil 判空回落内存模式）。
func TestNewRedisCmdsNil(t *testing.T) {
	assert.Nil(t, newRedisCmds(nil))
}
