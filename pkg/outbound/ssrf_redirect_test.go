package outbound

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/pigeonbox/core/conf"
)

func mustReq(t *testing.T, raw string) *http.Request {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url 解析失败 %q: %v", raw, err)
	}
	return &http.Request{URL: u}
}

// TestSSRFRedirectGuard（core#116 正判据）：重定向目标逐跳过 SSRF 校验——
// 私网/链路本地/环回目标与非 http(s) scheme 一律拒，公网目标放行，超过跳数中止。
//
// 判据为结构性 errors.Is/err==nil，不依赖本地化文案。
//
// 负对照（改错转红）：把 ssrfRedirectGuard 改成恒 nil（= 回到 DefaultClient
// 盲跟重定向）⇒ 拒绝分支的三条用例全部转红。
func TestSSRFRedirectGuard(t *testing.T) {
	// 显式关掉全局私网放行（本用例判的就是严格公网口径；判据不能随全局状态漂移）
	old := conf.GetGlobalConfig()
	cfg := &conf.AppConfiguration{}
	conf.SetGlobalConfig(cfg)
	defer conf.SetGlobalConfig(old)

	via := []*http.Request{mustReq(t, "https://peer.invalid/api/v1/p2p/invite")}

	for _, raw := range []string{
		"http://169.254.169.254/latest/meta-data/", // 链路本地（云元数据，issue 场景）
		"http://10.0.0.1/internal",                 // RFC1918 私网
		"http://127.0.0.1:8080/admin",              // 环回
		"file:///etc/passwd",                       // 非 http(s) scheme
	} {
		if err := ssrfRedirectGuard(mustReq(t, raw), via); err == nil {
			t.Fatalf("重定向到 %q 必须被拒绝", raw)
		}
	}

	// 公网目标放行（.invalid TLD 恒 NXDOMAIN ⇒ 不依赖真实 DNS）
	if err := ssrfRedirectGuard(mustReq(t, "https://other.invalid/ok"), via); err != nil {
		t.Fatalf("公网重定向目标不应被拒: %v", err)
	}

	// 跳数上限（与 TargetNotify 同口径 5 跳）
	longVia := make([]*http.Request, maxRedirects)
	for i := range longVia {
		longVia[i] = mustReq(t, "https://peer.invalid/hop")
	}
	if err := ssrfRedirectGuard(mustReq(t, "https://other.invalid/ok"), longVia); err == nil {
		t.Fatal("超过跳数上限必须中止")
	}
}

// TestOIDCOutboundPolicy（core#164 正判据）：TargetOIDC 曾是策略表唯一无
// checkRedirect 的出站点（Go 默认盲跟 10 跳）——必须挂逐跳 SSRF 复验守卫
// （与 federation 各目标同族，见 ssrf_redirect.go 口径说明）。
//
// 负对照（改错转红）：把 policies[TargetOIDC].checkRedirect 置回 nil
// ⇒ 本用例转红（回到盲跟重定向）。
func TestOIDCOutboundPolicy(t *testing.T) {
	if policies[TargetOIDC].checkRedirect == nil {
		t.Fatal("TargetOIDC 必须挂重定向守卫（core#164）")
	}
}

// TestFederationOutboundPolicies（core#116 正判据）：联邦两处出站的客户端必须
// 带超时（原 http.DefaultClient 零超时 ⇒ goroutine 泄漏面）且挂重定向守卫。
//
// 负对照：把 policies 对应条目的 timeout 归零 / checkRedirect 置 nil ⇒ 转红。
func TestFederationOutboundPolicies(t *testing.T) {
	for _, tc := range []struct {
		target  Target
		timeout time.Duration
	}{
		{TargetFederationInvite, 10 * time.Second},   // 迁移自 pushInvite 原 10s 预算
		{TargetFederationDirectory, 5 * time.Second}, // 迁移自 directory.go 原 5s 预算
	} {
		c := New(tc.target)
		if c.Timeout != tc.timeout {
			t.Fatalf("%s 超时口径不符: got=%v want=%v", tc.target, c.Timeout, tc.timeout)
		}
		if c.CheckRedirect == nil {
			t.Fatalf("%s 必须挂重定向逐跳复验守卫（nil = 盲跟重定向）", tc.target)
		}
	}
}
