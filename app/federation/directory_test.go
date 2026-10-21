package federation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDirectoryPeers 名录透传:路径拼接 + 响应解析 + 配置缺失。
//
// 2026-10-22 随目录实现搬入本包（目录属联邦域）。
func TestDirectoryPeers(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"nodes":[{"node_id":"n1","name":"站A","url":"https://a.example.com","box_pub":"aabb","online_since":1700000000}]}`))
	}))
	defer srv.Close()

	d := NewPeerDirectory(func(context.Context) (string, bool) { return srv.URL, true })
	peers, err := d.Peers(context.Background())
	if err != nil {
		t.Fatalf("名录查询失败: %v", err)
	}
	if gotPath != "/v1/directory" {
		t.Fatalf("名录路径错: %q", gotPath)
	}
	if len(peers) != 1 {
		t.Fatalf("对端数不对: %d", len(peers))
	}
	p := peers[0]
	if p.NodeID != "n1" || p.Name != "站A" || p.URL != "https://a.example.com" ||
		p.BoxPub != "aabb" || p.OnlineSince != 1700000000 {
		t.Fatalf("对端字段不符: %+v", p)
	}
}

// TestDirectoryConfigMissing 配置缺失时报错（不静默返空）。
func TestDirectoryConfigMissing(t *testing.T) {
	d := NewPeerDirectory(func(context.Context) (string, bool) { return "", false })
	if _, err := d.Peers(context.Background()); err == nil {
		t.Fatal("配置缺失应报错")
	}
}

// TestDirectoryUpstreamError 上游非 200 / 响应不可解析时报错。
func TestDirectoryUpstreamError(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer bad.Close()
	d := NewPeerDirectory(func(context.Context) (string, bool) { return bad.URL, true })
	if _, err := d.Peers(context.Background()); err == nil {
		t.Fatal("上游 502 应报错")
	}

	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not-json"))
	}))
	defer garbage.Close()
	d2 := NewPeerDirectory(func(context.Context) (string, bool) { return garbage.URL, true })
	if _, err := d2.Peers(context.Background()); err == nil {
		t.Fatal("响应不可解析应报错")
	}
}

// TestDirectoryPrimaryRegistry 名录只打主注册中心（多主备取首个）。
//
// 联邦段的 registry_url 支持逗号分隔主备（写全推、读依次 failover）；名录查询
// 只打主库——归一语义下沉到 pkg/registry（单源），此处断言联邦侧行为。
func TestDirectoryPrimaryRegistry(t *testing.T) {
	var primaryHits, backupHits int
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"nodes":[]}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"nodes":[]}`))
	}))
	defer backup.Close()

	d := NewPeerDirectory(func(context.Context) (string, bool) {
		return primary.URL + "," + backup.URL, true
	})
	if _, err := d.Peers(context.Background()); err != nil {
		t.Fatalf("名录查询失败: %v", err)
	}
	if primaryHits != 1 || backupHits != 0 {
		t.Fatalf("应只打主注册中心: primary=%d backup=%d", primaryHits, backupHits)
	}
}

// TestChainRegistryProvider 双源兜底：联邦段有值用联邦段，否则回退直传段；
// 两段皆无值 = 配置缺失（ok=false）。
func TestChainRegistryProvider(t *testing.T) {
	ctx := context.Background()
	fed := func(context.Context) (string, bool) { return "http://fed:12346", true }
	direct := func(context.Context) (string, bool) { return "http://direct:12346", true }

	// 联邦段有值 → 用联邦段
	if u, ok := ChainRegistryProvider(fed, direct)(ctx); !ok || u != "http://fed:12346" {
		t.Fatalf("联邦段优先被破: %q %v", u, ok)
	}
	// 联邦段无值 → 回退直传段（存量兼容）
	none := func(context.Context) (string, bool) { return "", false }
	if u, ok := ChainRegistryProvider(none, direct)(ctx); !ok || u != "http://direct:12346" {
		t.Fatalf("直传段兜底失效: %q %v", u, ok)
	}
	// 两段皆无 → 配置缺失
	if _, ok := ChainRegistryProvider(none, none)(ctx); ok {
		t.Fatal("两段皆无应判配置缺失")
	}
	// nil provider 跳过
	if u, ok := ChainRegistryProvider(nil, direct)(ctx); !ok || u != "http://direct:12346" {
		t.Fatalf("nil provider 应被跳过: %q %v", u, ok)
	}
}
