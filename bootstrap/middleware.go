package bootstrap

// 职责：HTTP 中间件——限流归桶（rateLimitBucket/rateLimitGlobalExempt）与 CORS 跨域。

import (
	"context"
	"net/url"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/pigeonbox/core/pkg/middleware"
)

// rateLimitBucket 路径 → 限流桶（"login"/"upload"/"download"，空串=不限）。
//
// 前缀必须与真实路由表逐一对应：TestRateLimitBucketCoversRoutes 守卫测试
// 用 hertz 实际注册路由回归本函数，任何"桶里写了、路由表没有"（死条目，
// 等于没限流）或"公开路由没进桶"都会在 CI 失败。
func rateLimitBucket(path string) string {
	switch {
	// 凭证类：爆破/灌号目标（注册与初始化向导同属此类，2026-10-05 审计补入；
	// OIDC login/callback 涉及 IdP 跳转与 code 换票建号，2026-10-06 补入）
	case strings.HasPrefix(path, "/admin/login"),
		strings.HasPrefix(path, "/user/login"),
		strings.HasPrefix(path, "/user/register"),
		strings.HasPrefix(path, "/api/v1/user/oidc/"),
		path == "/setup", path == "/setup/check":
		return "login"

	// 写入/上传类（匿名发布、分块上传、预签名、寄件码访客投递；MCP 快速分享
	// 同属写入通道——访客形态（mcp.allow_guest）下必须与网页匿名寄件同限流）
	case strings.HasPrefix(path, "/anonymous/generate"),
		strings.HasPrefix(path, "/anonymous/retrieve"),
		strings.HasPrefix(path, "/chunk/"),
		strings.HasPrefix(path, "/api/v1/presign"),
		strings.HasPrefix(path, "/api/v1/share/multi"),
		strings.HasPrefix(path, "/api/v1/request/"),
		// 跨站投递（联邦前台化，2026-11-01）：访客经本柜把文件转发到对方柜
		// ⇒ 与其它上传通道同桶（写入类：限流维度对齐）。
		path == "/api/v1/federation/deliver",
		path == "/api/v1/mcp",
		strings.HasPrefix(path, "/share/text"),
		strings.HasPrefix(path, "/share/file"):
		return "upload"

	// 下载与公开枚举类（/anonymous/search 与 /share/metadata 是免密码元数据
	// 预言机，/share/select 取件内容获取，/request 寄件码 token 探测，
	// /qrcode 图片编码 CPU 型公开端点，/api/v1/federation/resolve 可借本站
	// 做联邦枚举跳板）
	case strings.Contains(path, "/download"),
		strings.HasPrefix(path, "/anonymous/search/"),
		strings.HasPrefix(path, "/share/metadata/"),
		strings.HasPrefix(path, "/share/select"),
		strings.HasPrefix(path, "/request/"),
		strings.HasPrefix(path, "/preview/"),
		strings.HasPrefix(path, "/qrcode/"),
		strings.HasPrefix(path, "/openapi.json"),
		strings.HasPrefix(path, "/api/v1/federation/resolve"),
		// 跨站投递槽位（联邦前台化，2026-11-01）：公开铸槽位端点 ⇒ 与
		// /api/v1/p2p/invite 同款（防被人无限铸槽位耗资源）。
		path == "/api/v1/federation/delivery-slot",
		strings.HasPrefix(path, "/api/v1/p2p/invite"):
		return "download"
	}
	return ""
}

// rateLimitGlobalExempt 未归桶路径的全局限流豁免清单（纯函数，守卫测试覆盖）。
//
// 只豁免两类：① 健康探针（k8s/LB 节点 IP 高频调用，误封禁会让整个部署假死）；
// ② 哈希命名的静态构建产物与 favicon/robots（无业务逻辑，成本极低）。
// 其余未归桶路径一律走全局默认桶（rate_limit.global_qps，默认 100 QPS/IP）。
func rateLimitGlobalExempt(path string) bool {
	switch {
	case path == "/ping", path == "/health", path == "/live", path == "/ready",
		path == "/readyz", path == "/api/v1/ping",
		path == "/favicon.ico", path == "/robots.txt",
		strings.HasPrefix(path, "/assets/"):
		return true
	}
	return false
}

// CORS 跨域中间件（配置化）。
//
// 安全策略：
//   - 配置了 allow_origins 白名单时，仅放行白名单内的 Origin（生产推荐）
//   - 未配置白名单时，退化为反射 Origin（便于本地开发，等同于宽松模式）
//   - allow_credentials=true 时，绝不返回 "*"，而是精确匹配的 Origin
//
// 同时允许 X-Trace-Id / X-API-Key 等自定义请求头跨域。
// allow_origins 来源：yaml 的 security.cors.allow_origins（数组）或
// 环境变量 PB_CORS_ALLOW_ORIGINS（逗号分隔，如 "https://a.com,https://b.com"）。
func CORS() app.HandlerFunc {
	allowOrigins := map[string]bool{}
	// 白名单来自配置（bootstrap/config.go 已把 PB_CORS_ALLOW_ORIGINS 按 CSV 拆进
	// security.cors.allow_origins）——此前这里再读一次 env 是**冗余**（同样的值
	// 已经进配置），已删（2026-10-25 删除旧版本兼容）。
	// 再合并配置文件中的白名单
	for _, o := range config.Security.CORS.AllowOrigins {
		allowOrigins[o] = true
	}
	allowCredentials := config.Security.CORS.AllowCredentials
	// localhost 兜底仅限开发模式：生产实例放行任意 localhost Origin 等于给
	// 本机/内网里的恶意页面开跨域读通道（2026-10-06 审计收紧）。
	// 生产需要本地联调时用 PB_CORS_ALLOW_ORIGINS 显式加白名单。
	localhostFallback := config != nil && !config.IsProduction()

	return func(ctx context.Context, c *app.RequestContext) {
		origin := string(c.GetHeader("Origin"))

		allowedOrigin := ""
		if origin != "" {
			if allowOrigins[origin] {
				// 白名单精确匹配
				allowedOrigin = origin
			} else if localhostFallback && isLocalhostOrigin(origin) {
				// 无白名单或未命中白名单时，允许 localhost 跨域（开发友好）
				allowedOrigin = origin
			}
		}

		if allowedOrigin != "" {
			c.Header("Access-Control-Allow-Origin", allowedOrigin)
			c.Header("Vary", "Origin")
			if allowCredentials {
				// 凭证仅对允许的 origin 生效
				c.Header("Access-Control-Allow-Credentials", "true")
			}
		}

		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With, X-Trace-Id, X-API-Key")
		c.Header("Access-Control-Expose-Headers", "Content-Length, Content-Type, X-Trace-Id")
		c.Header("Access-Control-Max-Age", "86400")

		// 处理预检请求
		if string(c.Method()) == "OPTIONS" {
			c.AbortWithStatus(consts.StatusNoContent)
			return
		}

		c.Next(ctx)
	}
}

// mcpGuestGate MCP 端点请求闸门（2026-10-12 访客快速分享）：
//   - 浏览器跨站拒绝（mcpOriginAllowed）：allow_guest 形态下面向浏览器开放
//     JSON-RPC POST，须防 CSRF 借访客/管理员浏览器灌分享或删分享
//     （对标上游 v2.8 _validate_origin；原生 MCP 客户端无 Origin 头天然放行）；
//   - Cookie 认证的写请求补 CSRF 头校验（Bearer 天然免疫，viaCookie=false 直接过）。
func mcpGuestGate() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		if !mcpOriginAllowed(c) {
			c.JSON(consts.StatusForbidden, map[string]interface{}{
				"code":    consts.StatusForbidden,
				"message": "cross-site MCP requests are not allowed",
			})
			c.Abort()
			return
		}
		_, viaCookie := middleware.SessionToken(c)
		if !middleware.CSRFAllowed(c, viaCookie) {
			c.JSON(consts.StatusForbidden, map[string]interface{}{
				"code":    consts.StatusForbidden,
				"message": "缺少 CSRF 头（Cookie 认证的 MCP 请求须携带 X-Requested-With: XMLHttpRequest）",
			})
			c.Abort()
			return
		}
		c.Next(ctx)
	}
}

// mcpOriginAllowed 浏览器跨站判定：Sec-Fetch-Site: cross-site 或 Origin 与 Host
// 不符即拒绝；无 Origin/Sec-Fetch 头（Claude Desktop、curl 等原生客户端）放行。
func mcpOriginAllowed(c *app.RequestContext) bool {
	if string(c.GetHeader("Sec-Fetch-Site")) == "cross-site" {
		return false
	}
	origin := string(c.GetHeader("Origin"))
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == string(c.GetHeader("Host"))
}

// isLocalhostOrigin 判断是否 localhost/127.0.0.1 的任意端口（开发环境跨域放行）。
// 生产环境应通过 PB_CORS_ALLOW_ORIGINS 显式配置白名单。
func isLocalhostOrigin(origin string) bool {
	return strings.HasPrefix(origin, "http://localhost:") ||
		strings.HasPrefix(origin, "http://127.0.0.1:") ||
		strings.HasPrefix(origin, "https://localhost:") ||
		strings.HasPrefix(origin, "https://127.0.0.1:") ||
		origin == "http://localhost" || origin == "http://127.0.0.1" ||
		origin == "https://localhost" || origin == "https://127.0.0.1"
}
