package federation

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/security"
)

// TestPushInviteRedirectToPrivateRejected（core#116 正判据）：恶意对端 302 到
// 私网/环回地址 ⇒ 逐跳 SSRF 复验拒绝，且**第二跳一个请求都不发**（邀请口令
// 不外带）。
//
// 判据形状（结构性，不依赖文案）：
//  1. errors.Is(err, security.ErrEndpointURL)——错误链可判，不是文案匹配；
//  2. 内网靶子零命中；
//  3. 对端粒度 allow_private=true 只放行**登记端点本身**——302 目标仍按严格
//     公网口径复验（全局 allow_private_networks 全程关闭）。
//
// 负对照（改错转红）：把 outbound 的 ssrfRedirectGuard 换成恒 nil（或把
// TargetFederationInvite.checkRedirect 置 nil = 回到 DefaultClient 盲跟）⇒
// 客户端跟随 302 ⇒ ① 转红（err==nil）、② 转红（命中 1，口令已 POST 出去）。
func TestPushInviteRedirectToPrivateRejected(t *testing.T) {
	restore := allowPrivateNetworks(t, false)
	defer restore()

	var innerHits atomic.Int64
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		innerHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer inner.Close()

	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, inner.URL+"/steal", http.StatusFound)
	}))
	defer peer.Close()

	err := PushInviteToTarget(context.Background(),
		conf.FederationSyncTarget{URL: peer.URL, AllowPrivate: true},
		"AAAA-AAAA-AAAA", "发送站A", "")
	if err == nil || !errors.Is(err, security.ErrEndpointURL) {
		t.Fatalf("302 到环回地址必须被逐跳 SSRF 校验拒绝: %v", err)
	}
	if got := innerHits.Load(); got != 0 {
		t.Fatalf("被拒后第二跳不得出站: hits=%d，期望 0（口令不外带）", got)
	}
}

// TestPushInviteRedirectAllowedUnderGlobalFlag 守卫不误伤：全局
// security.ssrf.allow_private_networks 显式放行时，重定向到私网目标照常跟随
// （与 TargetNotify 重定向守卫同口径——尊重全局开关）。
//
// 负对照：守卫若无视全局开关一律拒 ⇒ 本用例转红。
func TestPushInviteRedirectAllowedUnderGlobalFlag(t *testing.T) {
	restore := allowPrivateNetworks(t, true)
	defer restore()

	var innerHits atomic.Int64
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		innerHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer inner.Close()

	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, inner.URL+"/moved", http.StatusFound)
	}))
	defer peer.Close()

	if err := PushInvite(context.Background(), peer.URL, "AAAA-AAAA-AAAA", "", ""); err != nil {
		t.Fatalf("全局放行时重定向应正常跟随: %v", err)
	}
	if got := innerHits.Load(); got != 1 {
		t.Fatalf("应恰好跟随一次: hits=%d", got)
	}
}

// TestDirectoryPeersRedirectToPrivateRejected（core#116 正判据）：名录查询的
// 302 到环回地址同样逐跳复验拒绝、第二跳零出站。
//
// 负对照同 TestPushInviteRedirectToPrivateRejected：去掉守卫 ⇒ 盲跟 ⇒ 转红。
func TestDirectoryPeersRedirectToPrivateRejected(t *testing.T) {
	restore := allowPrivateNetworks(t, false)
	defer restore()

	var innerHits atomic.Int64
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		innerHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"nodes":[]}`))
	}))
	defer inner.Close()

	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, inner.URL+"/v1/directory", http.StatusFound)
	}))
	defer peer.Close()

	d := NewPeerDirectory(func(context.Context) (string, bool) { return peer.URL, true })
	_, err := d.Peers(context.Background())
	if err == nil || !errors.Is(err, security.ErrEndpointURL) {
		t.Fatalf("302 到环回地址必须被逐跳 SSRF 校验拒绝: %v", err)
	}
	if got := innerHits.Load(); got != 0 {
		t.Fatalf("被拒后第二跳不得出站: hits=%d，期望 0", got)
	}
}
