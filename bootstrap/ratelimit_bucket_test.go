package bootstrap

import (
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/gen/router"
)

// TestRateLimitBucketCoversRoutes 限流矩阵守卫（2026-10-06 审计产物的永久回归）：
// 此前 /api/v1/chunk 前缀在路由表中不存在，分块上传一直没进限流桶（死条目 =
// 等于没限流），且 /qrcode、/share/select、/request 等公开端点从未入桶。
// 本测试双向守卫：
//  1. rateLimitBucket 使用的每个前缀必须命中 ≥1 条真实注册路由（防死条目）；
//  2. 每条真实注册路由要么进桶、要么在"无需限流"白名单内（防漏网裸奔端点）。
//
// 新增公开路由时若落 default 桶，请要么补进 rateLimitBucket，要么在
// noBucketAllowed 中给出放行理由。
func TestRateLimitBucketCoversRoutes(t *testing.T) {
	prev := config
	config = &conf.AppConfiguration{
		MCP:      conf.MCPConfig{Enabled: true},
		Security: conf.SecurityConfig{OIDC: conf.OIDCConfig{Enabled: true}},
		UI:       conf.UIConfig{ExposeOpenAPI: true},
	}
	defer func() { config = prev }()

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	router.GeneratedRegister(h)
	customizedRegister(h)

	type route struct{ method, path string }
	var actual []route
	for _, r := range h.Routes() {
		actual = append(actual, route{r.Method, r.Path})
	}
	if len(actual) == 0 {
		t.Fatal("路由表为空，测试环境装配失败")
	}

	// ---- 正向：桶内前缀必须命中真实路由（死条目检测）----
	// 与 rateLimitBucket 的 case 列表保持一致（不含 Contains("/download") 这类
	// 模糊匹配与精确匹配项，它们单独断言）。
	prefixes := []string{
		// login
		"/admin/login", "/user/login",
		"/user/register", "/api/v1/user/oidc/",
		// upload
		"/anonymous/generate", "/anonymous/retrieve",
		"/chunk/", "/api/v1/presign", "/api/v1/share/multi",
		"/api/v1/request/", "/share/text", "/share/file",
		// download
		"/anonymous/search/", "/share/metadata/", "/share/select",
		"/api/v1/p2p/invite",
		"/request/", "/preview/", "/qrcode/", "/api/v1/federation/resolve",
	}
	for _, p := range prefixes {
		found := false
		for _, r := range actual {
			if strings.HasPrefix(r.path, p) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("限流桶前缀 %q 未命中任何真实路由（死条目=该类端点实际未限流），请修正前缀或删除", p)
		}
	}

	// ---- 反向：所有路由要么进桶，要么在白名单内（漏网检测）----
	// 白名单条目必须给理由。注意自 2026-10-08 起，未进桶且不在
	// rateLimitGlobalExempt 豁免清单的路径会落全局默认桶（global_qps 兜底限流），
	// 这里"无需限流"的白名单语义已弱化为"无需专属桶"。
	noBucketAllowed := []string{
		// 健康探针（k8s/LB 高频调用，成本低）——同时是全局桶豁免项
		"/health", "/live", "/ready", "/readyz", "/ping", "/api/v1/ping",
		// 管理员凭证门控（/version 由 bootstrap 中间件收权；/api/v1/mcp 自带 AdminMiddleware）
		"/version", "/api/v1/mcp",
		// 低成本元信息/静态
		"/api/config", "/robots.txt", "/assets/",
		// 认证后端点（登录态/API Key 是前提，专属 IP 桶意义有限；落全局默认桶）
		"/admin/", "/user/", "/api/v1/user/", "/notifies/", "/api/v1/notifies/",
	}
	isNoBucket := func(p string) bool {
		for _, a := range noBucketAllowed {
			if p == a || strings.HasPrefix(p, a) {
				return true
			}
		}
		return false
	}

	for _, r := range actual {
		b := rateLimitBucket(r.path)
		switch b {
		case "login", "upload", "download":
			// 已进桶
		case "":
			if !isNoBucket(r.path) {
				t.Errorf("路由 %s %s 未进任何限流桶也不在白名单：新增公开端点必须选择限流维度（rateLimitBucket）或给出放行理由（noBucketAllowed）", r.method, r.path)
			}
		default:
			t.Errorf("rateLimitBucket(%q) 返回未知桶 %q", r.path, b)
		}
	}

	// ---- 关键语义断言（防前缀排序回归）----
	if got := rateLimitBucket("/chunk/upload/init/"); got != "upload" {
		t.Errorf("/chunk/upload/* 必须进 upload 桶，got %q（2026-10-06 死条目回归）", got)
	}
	if got := rateLimitBucket("/api/v1/request/abc/upload"); got != "upload" {
		t.Errorf("寄件码访客投递必须进 upload 桶，got %q", got)
	}
	if got := rateLimitBucket("/request/abc"); got != "download" {
		t.Errorf("寄件码 token 探测必须进 download 桶，got %q", got)
	}

	// ---- 全局兜底桶豁免清单断言（2026-10-08 GlobalMiddleware 接线配套）----
	// 探针/静态资源必须豁免（k8s 探针被封禁会让部署假死）
	for _, p := range []string{"/ping", "/health", "/live", "/ready", "/readyz",
		"/api/v1/ping", "/favicon.ico", "/robots.txt", "/assets/index-abc123.js"} {
		if !rateLimitGlobalExempt(p) {
			t.Errorf("路径 %q 应在全局限流豁免清单内（探针/静态资源误封禁 = 部署假死）", p)
		}
	}
	// 公开动态端点不得豁免：未归专属桶时必须吃全局默认桶
	for _, p := range []string{"/api/config", "/api/v1/notifies/public",
		"/api/v1/user/refresh", "/api/v1/user/logout", "/api/v1/mcp", "/version"} {
		if rateLimitGlobalExempt(p) {
			t.Errorf("路径 %q 不应在全局限流豁免清单内（公开/低门槛端点须有默认限流兜底）", p)
		}
	}
}

// TestOpenAPIRegisterGatedByConfig ui.expose_openapi=false 时 /openapi.json 不注册
func TestOpenAPIRegisterGatedByConfig(t *testing.T) {
	prev := config
	config = &conf.AppConfiguration{
		MCP:      conf.MCPConfig{Enabled: true},
		Security: conf.SecurityConfig{OIDC: conf.OIDCConfig{Enabled: true}},
		UI:       conf.UIConfig{ExposeOpenAPI: false},
	}
	defer func() { config = prev }()

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	router.GeneratedRegister(h)
	customizedRegister(h)
	for _, r := range h.Routes() {
		if r.Path == "/openapi.json" {
			t.Fatal("ui.expose_openapi=false 时 /openapi.json 不应注册")
		}
	}
}
