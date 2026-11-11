package federation

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/pigeonbox/core/pkg/security"
)

// TestRegistryRedirectToPrivateRejected（core#140 正判据）：registry 出站
// （注册/心跳/公告/resolve 共用的 TargetFederation）此前 checkRedirect=nil =
// Go 默认盲跟 10 跳——registry 回 302 到内网即 302 绕过面（core#116 同族）。
// 现挂 ssrfRedirectGuard 逐跳复验：302 到环回地址必须被拒，且第二跳零出站。
//
// 判据形状（结构性，不依赖文案）：
//  1. errors.Is(err, security.ErrEndpointURL)——错误链可判；
//  2. 内网靶子零命中。
//
// 负对照（改错转红）：把 outbound policies[TargetFederation].checkRedirect 置回
// nil ⇒ 盲跟 302 ⇒ ① 转红（err==nil）、② 转红（靶子命中 1）。
func TestRegistryRedirectToPrivateRejected(t *testing.T) {
	restore := allowPrivateNetworks(t, false)
	defer restore()

	var innerHits atomic.Int64
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		innerHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"node_id":"x","url":"https://n.example","name":"n","expires_at":0,"size_hint":0}`))
	}))
	defer inner.Close()

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, inner.URL+"/steal", http.StatusFound)
	}))
	defer registry.Close()

	c := newRegistryClient(registry.URL)
	if _, err := c.resolve(context.Background(), "AAAA-AAAA-AAAA"); err == nil || !errors.Is(err, security.ErrEndpointURL) {
		t.Fatalf("registry 302 到环回地址必须被逐跳 SSRF 校验拒绝: %v", err)
	}
	if got := innerHits.Load(); got != 0 {
		t.Fatalf("被拒后第二跳不得出站: hits=%d，期望 0", got)
	}
}

// TestRegistryRedirectAllowedUnderGlobalFlag 守卫不误伤：全局
// security.ssrf.allow_private_networks 显式放行时（与 registry_url 配置期校验
// 同口径），重定向到私网目标照常跟随。
//
// 负对照：守卫若无视全局开关一律拒 ⇒ 本用例转红。
func TestRegistryRedirectAllowedUnderGlobalFlag(t *testing.T) {
	restore := allowPrivateNetworks(t, true)
	defer restore()

	var innerHits atomic.Int64
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		innerHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"node_id":"x","url":"https://n.example","name":"n","expires_at":0,"size_hint":0}`))
	}))
	defer inner.Close()

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, inner.URL+"/moved", http.StatusFound)
	}))
	defer registry.Close()

	c := newRegistryClient(registry.URL)
	info, err := c.resolve(context.Background(), "AAAA-AAAA-AAAA")
	if err != nil {
		t.Fatalf("全局放行时重定向应正常跟随: %v", err)
	}
	if info == nil || info.NodeID != "x" {
		t.Fatalf("应解析到重定向后的响应: %+v", info)
	}
	if got := innerHits.Load(); got != 1 {
		t.Fatalf("应恰好跟随一次: hits=%d，期望 1", got)
	}
}
