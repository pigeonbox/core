package updatecheck

// 匿名安装分布上报（hub#385）单测：默认关闭 + 七个字段契约。
//
// 断言是结构事实：请求是否发生、body 的字段集合与值；不依赖文案。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestReportDisabledByDefault：ReportURL 为空时完全不发请求。
func TestReportDisabledByDefault(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer srv.Close()
	// Endpoint 指向一个假上游（返回合法 release JSON）；ReportURL 为空。
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9","html_url":"https://example/releases/9.9.9"}`))
	}))
	defer upstream.Close()
	c := New(Config{Enabled: true, Current: "5.8.0", Endpoint: upstream.URL, TTL: time.Hour})
	_ = c.Check(t.Context(), true)
	time.Sleep(50 * time.Millisecond) // 给异步上报留窗口（本用例期望零请求）
	if hits != 0 {
		t.Fatalf("ReportURL 为空时不应上报, got %d", hits)
	}
}

// TestReportPayloadContract：开启后恰好一条，字段集合=契约（七个字段）。
func TestReportPayloadContract(t *testing.T) {
	type got struct {
		Body map[string]any
		CT   string
	}
	ch := make(chan got, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		ch <- got{Body: body, CT: r.Header.Get("Content-Type")}
	}))
	defer srv.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9","html_url":"https://example/releases/9.9.9"}`))
	}))
	defer upstream.Close()
	c := New(Config{
		Enabled: true, Current: "5.8.0", Endpoint: upstream.URL, TTL: time.Hour,
		ReportURL: srv.URL, InstanceID: "0123456789abcdef0123456789abcdef",
		UpgradeFrom: "5.7.0", SchemaVersion: "000014",
	})
	_ = c.Check(t.Context(), true)
	select {
	case g := <-ch:
		if g.CT != "application/json" {
			t.Fatalf("Content-Type = %q", g.CT)
		}
		want := map[string]bool{
			"version": true, "platform": true, "arch": true,
			"upgrade_from": true, "schema_version": true,
			"instance_id": true, "checked_at": true,
		}
		for k := range g.Body {
			if !want[k] {
				t.Fatalf("上报出现契约外字段 %q: %v", k, g.Body)
			}
			delete(want, k)
		}
		if len(want) != 0 {
			t.Fatalf("上报缺字段 %v: %v", want, g.Body)
		}
		if g.Body["version"] != "5.8.0" || g.Body["upgrade_from"] != "5.7.0" ||
			g.Body["schema_version"] != "000014" || g.Body["instance_id"] != "0123456789abcdef0123456789abcdef" {
			t.Fatalf("字段值不符: %v", g.Body)
		}
		if g.Body["platform"] == "" || g.Body["arch"] == "" {
			t.Fatalf("platform/arch 不应为空: %v", g.Body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("上报请求未到达")
	}
	// 同一检查周期内只报一次（缓存命中不再上报）。
	_ = c.Check(t.Context(), false)
	time.Sleep(50 * time.Millisecond)
	select {
	case <-ch:
		t.Fatal("缓存命中时不应再次上报")
	default:
	}
}

// TestReportInvalidURLSkips：非法/非 https 的 ReportURL 直接跳过（不 panic）。
func TestReportInvalidURLSkips(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9"}`))
	}))
	defer upstream.Close()
	c := New(Config{Enabled: true, Current: "5.8.0", Endpoint: upstream.URL,
		ReportURL: "http://insecure.example/collect", InstanceID: "x"})
	_ = c.Check(t.Context(), true) // 不应 panic/阻塞
}
