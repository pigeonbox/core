package updatecheck

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// releaseServer 起一个假的上游，返回指定 tag。
func releaseServer(t *testing.T, tag string, hits *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			atomic.AddInt32(hits, 1)
		}
		_ = json.NewEncoder(w).Encode(githubRelease{
			TagName: tag,
			HTMLURL: "https://example.test/releases/" + tag,
		})
	}))
}

func newChecker(t *testing.T, endpoint, current string) *Checker {
	t.Helper()
	return New(Config{
		Enabled:   true,
		Endpoint:  endpoint,
		Current:   current,
		CachePath: filepath.Join(t.TempDir(), cacheFileName),
	})
}

func TestDisabledShortCircuits(t *testing.T) {
	var hits int32
	srv := releaseServer(t, "v5.0.0", &hits)
	defer srv.Close()

	c := New(Config{Enabled: false, Endpoint: srv.URL, Current: "4.9.1"})
	res := c.Check(context.Background(), false)

	assert.Equal(t, SourceDisabled, res.Source)
	assert.False(t, res.Available)
	assert.Zero(t, atomic.LoadInt32(&hits), "关闭时不得发出任何出站请求")
}

func TestNewerVersionDetected(t *testing.T) {
	srv := releaseServer(t, "v5.0.0", nil)
	defer srv.Close()

	res := newChecker(t, srv.URL, "4.9.1").Check(context.Background(), false)

	require.Equal(t, SourceGitHub, res.Source)
	assert.Equal(t, "v5.0.0", res.Latest)
	assert.True(t, res.Available)
	assert.Contains(t, res.URL, "v5.0.0")
	assert.Empty(t, res.Error)
}

func TestSameVersionNotAvailable(t *testing.T) {
	srv := releaseServer(t, "v4.9.1", nil)
	defer srv.Close()

	res := newChecker(t, srv.URL, "4.9.1").Check(context.Background(), false)
	assert.Equal(t, SourceGitHub, res.Source)
	assert.False(t, res.Available)
}

// 负对照：构建版本号可能是 v4.0.7-1-gb8c15f1（真机实录），
// 不能因为字符串形态特殊就误报「有新版本」。
func TestNonSemverCurrentDoesNotMisreport(t *testing.T) {
	srv := releaseServer(t, "v4.9.1", nil)
	defer srv.Close()

	res := newChecker(t, srv.URL, "v4.0.7-1-gb8c15f1").Check(context.Background(), false)
	require.Equal(t, SourceGitHub, res.Source)
	assert.True(t, res.Available, "4.0.7 → 4.9.1 应当判为有更新")

	res2 := newChecker(t, srv.URL, "not-a-version").Check(context.Background(), false)
	assert.False(t, res2.Available, "无法解析的当前版本不得误报")
}

func TestOfflineYieldsUnknownNotFailure(t *testing.T) {
	// 指向一个必然拒绝连接的端口。
	c := New(Config{
		Enabled:  true,
		Endpoint: "http://127.0.0.1:1/latest",
		Current:  "4.9.1",
		Timeout:  200 * time.Millisecond,
	})
	res := c.Check(context.Background(), false)

	assert.Equal(t, SourceUnknown, res.Source)
	assert.False(t, res.Available)
	assert.NotEmpty(t, res.Error, "未知状态必须给出可读原因")
}

func TestNonHTTPSRejected(t *testing.T) {
	c := New(Config{Enabled: true, Endpoint: "http://evil.example.com/latest", Current: "4.9.1"})
	res := c.Check(context.Background(), false)
	require.Equal(t, SourceUnknown, res.Source)
	assert.Contains(t, res.Error, "https")
}

func TestCacheAvoidsSecondRequest(t *testing.T) {
	var hits int32
	srv := releaseServer(t, "v5.0.0", &hits)
	defer srv.Close()

	c := newChecker(t, srv.URL, "4.9.1")
	first := c.Check(context.Background(), false)
	second := c.Check(context.Background(), false)

	assert.Equal(t, SourceGitHub, first.Source)
	assert.Equal(t, SourceGitHub, second.Source)
	assert.Equal(t, int32(1), atomic.LoadInt32(&hits), "24h 缓存内不得重复请求上游")

	// force=true 必须真的再打一次（管理台「立即检查」）。
	_ = c.Check(context.Background(), true)
	assert.Equal(t, int32(2), atomic.LoadInt32(&hits))
}

func TestCacheSurvivesRestart(t *testing.T) {
	var hits int32
	srv := releaseServer(t, "v5.0.0", &hits)
	defer srv.Close()

	cachePath := filepath.Join(t.TempDir(), cacheFileName)
	cfg := Config{Enabled: true, Endpoint: srv.URL, Current: "4.9.1", CachePath: cachePath}
	require.Equal(t, SourceGitHub, New(cfg).Check(context.Background(), false).Source)

	// 模拟进程重启：新实例应直接吃落盘缓存，不打上游。
	res := New(cfg).Check(context.Background(), false)
	assert.Equal(t, SourceGitHub, res.Source)
	assert.True(t, res.Available)
	assert.Equal(t, int32(1), atomic.LoadInt32(&hits), "重启后应复用落盘缓存")
}

func TestStaleCacheServedOnFailure(t *testing.T) {
	var hits int32
	srv := releaseServer(t, "v5.0.0", &hits)
	c := newChecker(t, srv.URL, "4.9.1")
	require.Equal(t, SourceGitHub, c.Check(context.Background(), false).Source)

	// 上游消失 + 缓存判过期 ⇒ 应带旧值并标 stale，而不是退回 unknown。
	c.cfg.TTL = time.Nanosecond
	srv.Close()

	res := c.Check(context.Background(), true)
	assert.Equal(t, SourceGitHub, res.Source)
	assert.True(t, res.Available)
	assert.True(t, res.Stale, "过期缓存必须显式标记")
	assert.NotEmpty(t, res.Error)
}

func TestConcurrentCallsCollapseToOneRequest(t *testing.T) {
	var hits int32
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		<-block // 卡住，让并发调用都落到同一次请求上
		_ = json.NewEncoder(w).Encode(githubRelease{TagName: "v5.0.0"})
	}))
	defer srv.Close()

	c := newChecker(t, srv.URL, "4.9.1")
	var wg sync.WaitGroup
	results := make([]Result, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = c.Check(context.Background(), false)
		}(i)
	}
	time.Sleep(50 * time.Millisecond) // 让所有 goroutine 进入合并分支
	close(block)
	wg.Wait()

	assert.Equal(t, int32(1), atomic.LoadInt32(&hits), "并发调用必须合并成一次上游请求")
	for i, r := range results {
		assert.Equal(t, SourceGitHub, r.Source, "第 %d 个调用应拿到同一结果", i)
	}
}

func TestMalformedUpstreamDegrades(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{not json"))
	}))
	defer srv.Close()

	res := newChecker(t, srv.URL, "4.9.1").Check(context.Background(), false)
	assert.Equal(t, SourceUnknown, res.Source)
	assert.NotEmpty(t, res.Error)
}

func TestCorruptedCacheIgnored(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, cacheFileName)
	require.NoError(t, os.WriteFile(cachePath, []byte("{half"), 0o600))

	var hits int32
	srv := releaseServer(t, "v5.0.0", &hits)
	defer srv.Close()

	c := New(Config{Enabled: true, Endpoint: srv.URL, Current: "4.9.1", CachePath: cachePath})
	res := c.Check(context.Background(), false)
	assert.Equal(t, SourceGitHub, res.Source)
	assert.Equal(t, int32(1), atomic.LoadInt32(&hits), "坏缓存应被忽略并重新查询")
}

func TestIsNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"v5.0.0", "4.9.1", true},
		{"v4.9.2", "4.9.1", true},
		{"v4.10.0", "4.9.9", true},
		{"4.9.1", "v4.9.1", false},
		{"v4.9.0", "4.9.1", false},
		{"v4.0.7-1-gb8c15f1", "4.9.1", false},
		{"garbage", "4.9.1", false},
		{"v5.0.0", "dev", false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, isNewer(tc.latest, tc.current), "isNewer(%q, %q)", tc.latest, tc.current)
	}
}

func TestParseSemver(t *testing.T) {
	got, ok := parseSemver("v4.0.7-1-gb8c15f1")
	require.True(t, ok)
	assert.Equal(t, [3]int{4, 0, 7}, got)

	got, ok = parseSemver("5")
	require.True(t, ok)
	assert.Equal(t, [3]int{5, 0, 0}, got)

	_, ok = parseSemver("")
	assert.False(t, ok)
}
