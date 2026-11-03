package gate

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/pigeonbox/core/conf"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetQuotaMem 重置进程内计数器（测试隔离）
func resetQuotaMem(t *testing.T) {
	t.Helper()
	SetQuotaRedis(nil)
	quota.mu.Lock()
	quota.mem = map[string]*quotaEntry{}
	quota.memDate = ""
	quota.mu.Unlock()
}

func withQuotaConf(t *testing.T, count, bytes int64) {
	t.Helper()
	withConf(t, &conf.AppConfiguration{Upload: conf.UploadConfig{
		OpenUpload:          true,
		AnonymousDailyCount: count,
		AnonymousDailyBytes: bytes,
	}})
	t.Cleanup(func() { resetQuotaMem(t) })
}

func TestCheckAnonymousQuota_Disabled(t *testing.T) {
	withQuotaConf(t, 0, 0)
	assert.NoError(t, CheckAnonymousQuota(context.Background(), "1.2.3.4", 1<<20))
}

func TestCheckAnonymousQuota_CountLimit(t *testing.T) {
	withQuotaConf(t, 3, 0)
	resetQuotaMem(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		require.NoError(t, CheckAnonymousQuota(ctx, "1.2.3.4", 10))
	}
	err := CheckAnonymousQuota(ctx, "1.2.3.4", 10)
	require.Error(t, err)
	ge, ok := err.(*GateError)
	require.True(t, ok)
	assert.Equal(t, 10014, ge.ErrCode())

	// 不同 IP 各自独立计数
	assert.NoError(t, CheckAnonymousQuota(ctx, "5.6.7.8", 10))
}

func TestCheckAnonymousQuota_BytesLimit(t *testing.T) {
	withQuotaConf(t, 0, 1000)
	resetQuotaMem(t)
	ctx := context.Background()
	require.NoError(t, CheckAnonymousQuota(ctx, "1.2.3.4", 600))
	require.NoError(t, CheckAnonymousQuota(ctx, "1.2.3.4", 400))
	err := CheckAnonymousQuota(ctx, "1.2.3.4", 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "流量已达上限")
}

func TestCheckAnonymousQuota_DateRollover(t *testing.T) {
	withQuotaConf(t, 1, 0)
	resetQuotaMem(t)
	ctx := context.Background()
	require.NoError(t, CheckAnonymousQuota(ctx, "1.2.3.4", 10))
	assert.Error(t, CheckAnonymousQuota(ctx, "1.2.3.4", 10))

	// 模拟跨天：手工推进内存计数器的日期
	quota.mu.Lock()
	quota.memDate = "20000101"
	quota.mu.Unlock()
	assert.NoError(t, CheckAnonymousQuota(ctx, "1.2.3.4", 10))
}

func TestDailyQuota_Concurrent(t *testing.T) {
	withQuotaConf(t, 100, 0)
	resetQuotaMem(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = CheckAnonymousQuota(ctx, "9.9.9.9", 1)
		}()
	}
	wg.Wait()
	quota.mu.Lock()
	defer quota.mu.Unlock()
	assert.Equal(t, int64(50), quota.mem["9.9.9.9"].count, "并发计数不丢")
}

// TestQuotaRedis_IncrWithTTLSemantics Redis 路径计数语义锁（计数原语收编到
// oekit/redisx `Counter.IncrWithTTL` 后，与 pkg/middleware 的语义锁同款）：
//   - 次数/流量两键独立累加；
//   - TTL 首次命中即设（= 当日结束 + 1h 缓冲的绝对过期）；
//   - 已有 TTL **不续期**（固定窗口；若每次续期，持续流量下当日键永不过期，
//     窗口语义会漂成滑动窗口）。
func TestQuotaRedis_IncrWithTTLSemantics(t *testing.T) {
	withQuotaConf(t, 100, 1<<30)
	s := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer func() { _ = client.Close() }()
	SetQuotaRedis(client)

	ctx := context.Background()
	day := time.Now().Format("20060102")
	ckey := "pb:quota:1.2.3.4:" + day + ":count"
	bkey := "pb:quota:1.2.3.4:" + day + ":bytes"

	require.NoError(t, CheckAnonymousQuota(ctx, "1.2.3.4", 100))
	require.NoError(t, CheckAnonymousQuota(ctx, "1.2.3.4", 200))

	cn, err := s.Get(ckey)
	require.NoError(t, err)
	assert.Equal(t, "2", cn, "次数累加")
	bn, err := s.Get(bkey)
	require.NoError(t, err)
	assert.Equal(t, "300", bn, "流量累加")

	first := s.TTL(ckey)
	assert.Greater(t, first, time.Duration(0), "首次命中必须设 TTL")
	assert.LessOrEqual(t, first, timeUntilMidnight()+time.Hour+time.Second,
		"TTL 上限=当日结束+1h 缓冲（+1s 断言余量）")

	// 时钟推进 10s 后再计数：TTL 不得被续期（固定窗口）
	s.FastForward(10 * time.Second)
	require.NoError(t, CheckAnonymousQuota(ctx, "1.2.3.4", 1))
	assert.LessOrEqual(t, s.TTL(ckey), first-10*time.Second+time.Second,
		"TTL 不得被续期")
}

// TestQuotaRedis_CountLimitReject Redis 路径的超限拒绝（配合上一条语义锁，
// 证明比较逻辑与 Redis 计数器联动正常）。
func TestQuotaRedis_CountLimitReject(t *testing.T) {
	withQuotaConf(t, 2, 0)
	s := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer func() { _ = client.Close() }()
	SetQuotaRedis(client)

	ctx := context.Background()
	require.NoError(t, CheckAnonymousQuota(ctx, "1.2.3.4", 1))
	require.NoError(t, CheckAnonymousQuota(ctx, "1.2.3.4", 1))
	err := CheckAnonymousQuota(ctx, "1.2.3.4", 1)
	require.Error(t, err)
	ge, ok := err.(*GateError)
	require.True(t, ok)
	assert.Equal(t, 10014, ge.ErrCode())
}
