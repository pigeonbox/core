package bootstrap

// 职责：自定义路由注册（不走 thrift IDL 生成）——customizedRegister 与
// SPA 静态资源回退（apiPathPrefixes/isAPIPath/staticFileExtensions/tryServeStatic）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"go.uber.org/zap"

	mcpApp "github.com/pigeonbox/core/app/mcp"
	oidcApp "github.com/pigeonbox/core/app/oidc"
	presignHandler "github.com/pigeonbox/core/gen/handler/presign"
	userHandler "github.com/pigeonbox/core/gen/handler/user"
	"github.com/pigeonbox/core/pkg/auth"
	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/pkg/middleware"
	customHandler "github.com/pigeonbox/core/transport/http/handler"
	customMw "github.com/pigeonbox/core/transport/http/middleware"
)

// customizedRegister 注册自定义路由（不走 thrift IDL 生成）。
//
// 历史背景：该函数此前为空，导致前端 SPA 静态服务、Swagger 文档、
// “我的分享管理”与“用户通知”REST API 全部未生效。本函数把此前残留在
// 根目录 router.go（死代码）中的逻辑正式接线，使单二进制 / Docker 部署
// 即可打开前端页面并使用全部功能。
func customizedRegister(r *server.Hertz) {
	// ===== OpenAPI 文档（Swagger UI）=====
	// ui.expose_openapi=false 时不注册（404）：端点全清单对攻击者是现成的
	// 侦察地图；前端 /api-docs 页是唯一消费方，生产不需要时可整体关闭。
	if config.UI.ExposeOpenAPI {
		r.GET("/openapi.json", customHandler.OpenAPISpec)
	}

	// ===== presign 预签名直传端点（gen router 未注册，在此补）=====（公开面）
	if config.ServesPublicPlane() {
		r.PUT("/api/v1/presign/upload-direct/:uploadID", presignHandler.UploadDirect)
	}

	// ===== token 刷新端点（前端 401 拦截器调用，换发新 token）=====
	// 令牌来源：Bearer 头或会话 Cookie；轮换同时下发新 Cookie（2026-10-05
	// 遗留修复：浏览器会话迁 HttpOnly Cookie）。Cookie 认证时须带 CSRF 头。
	r.POST("/api/v1/user/refresh", func(ctx context.Context, c *app.RequestContext) {
		oldToken, viaCookie := middleware.SessionToken(c)
		if oldToken == "" {
			c.JSON(consts.StatusUnauthorized, map[string]interface{}{"code": 401, "message": "missing token"})
			return
		}
		if !middleware.CSRFAllowed(c, viaCookie) {
			c.JSON(consts.StatusForbidden, map[string]interface{}{"code": 403, "message": "缺少 CSRF 头"})
			return
		}
		newToken, err := auth.RefreshToken(ctx, oldToken)
		if err != nil {
			middleware.ClearSessionCookie(c)
			c.JSON(consts.StatusUnauthorized, map[string]interface{}{"code": 401, "message": "token invalid or expired"})
			return
		}
		middleware.SetSessionCookie(c, newToken, int(auth.SessionExpiry().Seconds()))
		c.JSON(consts.StatusOK, map[string]interface{}{
			"code": 200, "message": "ok",
			"data": map[string]string{"token": newToken},
		})
	})

	// ===== 公开配置端点（前端 configStore 启动时拉取）=====
	// 前端 publicApi.getConfig() 请求 /api/config 获取站点配置（名称、上传限制等），
	// 此前端点缺失导致前端启动报 "获取配置失败: Network Error"。此处补齐。
	r.GET("/api/config", publicConfigHandler)

	// ===== P2P 联邦解析代理（M2；未启用时 handler 返回 available:false）=====（公开面）
	if config.ServesPublicPlane() {
		r.GET("/api/v1/federation/resolve", customHandler.FederationResolve)
	}

	// ===== robots.txt（对标上游 SEO 可配；输出 ui.robots_text）=====（公开面）
	if config.ServesPublicPlane() {
		r.GET("/robots.txt", func(ctx context.Context, c *app.RequestContext) {
			content := config.UI.RobotsText
			if content == "" {
				content = "User-agent: *\nDisallow: /\n"
			}
			c.Header("Content-Type", "text/plain; charset=utf-8")
			c.String(consts.StatusOK, content)
		})
	}

	// ===== 站级公告（匿名公开端点；对标上游首页通知条）=====
	// notify.Active 已过滤 target_user_id（仅全站公告）；前端 SiteNotice 组件消费
	r.GET("/api/v1/notifies/public", customHandler.ListPublicNotifies)

	// ===== MCP server（Model Context Protocol；AI 客户端集成，上游没有的差异化能力）=====
	// Streamable HTTP 传输：POST /api/v1/mcp（JSON-RPC 2.0），管理员 JWT 认证。
	// Claude Desktop 等标准客户端以 Authorization: Bearer <admin token> 接入。（管理面）
	if config.MCP.Enabled && config.ServesAdminPlane() {
		// initThriftIDLServices 未跑（如轻量测试环境）时惰性兜底：
		// share service 缺席时 share_text 工具会明确报错，协议处理不受影响
		if mcpService == nil {
			mcpService = mcpApp.NewService(config.App.Version)
		}
		r.POST("/api/v1/mcp", middleware.AdminMiddleware(), func(ctx context.Context, c *app.RequestContext) {
			body := c.Request.Body()
			status, respBody := mcpService.Handle(ctx, body)
			if status == 202 {
				c.SetStatusCode(consts.StatusAccepted)
				return
			}
			c.Header("Content-Type", "application/json")
			c.SetStatusCode(status)
			_, _ = c.Write(respBody)
		})
	}

	// ===== logout 端点（补齐基准 P1 缺口：显式注销 + token 黑名单）=====
	// 令牌来源：Bearer 头或会话 Cookie；同时清除 Cookie。
	r.POST("/api/v1/user/logout", func(ctx context.Context, c *app.RequestContext) {
		token, _ := middleware.SessionToken(c)
		if token == "" {
			c.JSON(consts.StatusUnauthorized, map[string]interface{}{"code": 401, "message": "missing token"})
			return
		}
		// 黑名单 TTL = token 剩余有效期（过期后自然失效，无需清理任务）
		if claims, err := auth.ParseToken(token); err == nil {
			remaining := time.Until(claims.ExpiresAt.Time)
			auth.RevokeToken(ctx, token, remaining)
		}
		middleware.ClearSessionCookie(c)
		c.JSON(consts.StatusOK, map[string]interface{}{"code": 200, "message": "已退出登录"})
	})

	// ===== 一键吊销全部 API Key（JWT-only：Key 不能管 Key；应急止损，见设计文档 §9.3）=====
	// 治理规则（§9.1-10）：向 /user/api-keys 或 /api/v1 组新增路由前，必须评估该路由的 API Key 暴露面
	r.POST("/user/api-keys/revoke-all", middleware.AuthMiddleware(), userHandler.RevokeAllAPIKeys)

	// ===== 多文件分享（P0 多文件，手写路由）：直传 / chunk+presign 绑定 =====
	// 可选身份（OptionalIdentity）：匿名可用，登录/带 Key 时注入 user_id 走配额与归属（公开面）
	if config.ServesPublicPlane() {
		multiShare := r.Group("/api/v1/share", middleware.OptionalIdentity()...)
		{
			multiShare.POST("/multi-direct", customHandler.MultiShareDirect)
			multiShare.POST("/multi-bind", customHandler.MultiShareBind)
		}

		// ===== 取件元数据（对标上游 /share/metadata：查询不扣次数、不要密码）=====
		r.GET("/share/metadata/:code", customHandler.ShareMetadata)
	}

	// ===== 寄件码/反向收件（P2）：2026-10-09 IDL 化（idl/request.thrift）， =====
	// 由 gen/router/request 注册（public/standalone 面；JWT 组 mw 手工区 +
	// 访客公开投递），customHandler 手写注册摘除；访客投递重管道桥接保留在
	// customHandler.GuestSubmitFiles（gen handler 委托），service 双注入见下方。

	// ===== NAS 本地文件免上传导入（P3；upload.local_import.enabled 开关在 service 内校验）=====（公开面）
	if config.ServesPublicPlane() {
		r.POST("/api/v1/user/shares/import-local", middleware.UserOrAPIKey(), customHandler.UserImportLocal)
	}

	// ===== OIDC 单点登录（常注册+运行时门控：handler 按 Enabled() 响应，
	// 管理端在线改 OIDC 段经 ReconfigureHooks 热重建 service，无需重启）=====
	if config != nil {
		customHandler.SetOIDCService(oidcApp.NewService(oidcApp.Config{
			Enabled:          config.Security.OIDC.Enabled,
			Issuer:           config.Security.OIDC.Issuer,
			ClientID:         config.Security.OIDC.ClientID,
			ClientSecret:     config.Security.OIDC.ClientSecret,
			Scopes:           config.Security.OIDC.Scopes,
			FrontendCallback: config.Security.OIDC.FrontendCallback,
		}))
	}
	// OIDC 登录/回调路由是访客登录入口（公开面）；service 注入保留无条件
	// （applyPropagatedConfig 热重建复用同一构造，admin 面不注册路由时注入无害）
	if config.ServesPublicPlane() {
		r.GET("/api/v1/user/oidc/login", customHandler.OIDCLogin)
		r.GET("/api/v1/user/oidc/callback", customHandler.OIDCCallback)
	}

	// ===== check-auth 端点（前端启动时校验 token 有效性并取回用户信息）=====
	r.GET("/api/v1/user/check-auth", customMw.UserAuth(), func(ctx context.Context, c *app.RequestContext) {
		uid, _ := c.Get("user_id")
		username, _ := c.Get("username")
		role, _ := c.Get("role")
		c.JSON(consts.StatusOK, map[string]interface{}{
			"code":    200,
			"message": "ok",
			"data": map[string]interface{}{
				"id":       uid,
				"username": username,
				"role":     role,
			},
		})
	})

	// ===== 管理端增强面（用户 CRUD / 文件管理 / 富统计 / 传输日志 / 分享治理 /
	// 用户配置 / 本地文件管理 / 设置测试端点 / 审计日志）——
	// 2026-10-10 分两波 IDL 化（idl/admin.thrift），由 gen/router/admin 注册
	//（standalone 走 GeneratedRegister 全量组合，admin 副本按部署模式 case 注册，
	// AdminMiddleware 组内），customHandler 手写注册全部摘除（wire 逐字段保形）。

	// ===== 前端构建产物静态资源 =====
	// Vite 输出的 index.html 用根级绝对路径引用资源（/assets/xxx、/vite.svg），
	// 故把 ./static/assets 挂到 /assets。用 StaticFS 正确处理 Range 请求、MIME、
	// 缓存（NoRoute 里手写的 c.File 对大文件 ES module 的 Range/缓冲处理不够稳定，
	// 会导致浏览器 "Failed to fetch dynamically imported module"）。
	r.StaticFS("/assets", &app.FS{
		Root:          filepath.Join(staticOpts.StaticDir, "assets"),
		PathRewrite:   app.NewPathSlashesStripper(1),
		CacheDuration: 7 * 24 * time.Hour,
	})

	// ===== Prometheus 指标端点（独立内网 server，默认不暴露到主端口）=====
	// 开启时绑定 127.0.0.1:9090（可用 PB_METRICS_ADDR 配置），供同节点 Prometheus 抓取。
	// 主 server 不注册 /metrics，避免公网泄露内部指标。
	if config.Observability.Metrics.Enabled {
		metricsPath := config.Observability.Metrics.Path
		if metricsPath == "" {
			metricsPath = "/metrics"
		}
		metricsAddr := os.Getenv("PB_METRICS_ADDR")
		if metricsAddr == "" {
			metricsAddr = "127.0.0.1:9090"
		}
		metricsServer := server.New(server.WithHostPorts(metricsAddr))
		metricsServer.GET(metricsPath, metricsHandler)
		go func() {
			logger.Info("metrics server listening", zap.String("addr", metricsAddr))
			if err := metricsServer.Run(); err != nil {
				logger.Error("metrics server failed", zap.Error(err))
			}
		}()
	}

	// ===== 深度就绪检查 =====
	// /readyz 检查 DB 等依赖连通性，供 K8s readinessProbe 使用；
	// 依赖不可用时返回 503，避免流量打到未就绪实例。
	// （IDL 生成的 /ready 为轻量 stub，此处用 /readyz 做深度检查以避免路由冲突）
	r.GET("/readyz", readinessHandler)

	// ===== 自定义 REST API（用户 JWT 或 API Key 认证）=====
	// 我的通知 mine 三件套 2026-10-09 IDL 化（idl/notify.thrift Mine/UnreadCount/
	// MarkRead），由 gen/router/notify 注册并挂 UserOrAPIKey（_apiMw 手工区），
	// customHandler 手写注册摘除（wire 形态逐字段兼容）。

	// ===== 前端 SPA 静态资源服务 =====
	// Vite 构建的 index.html 使用根级绝对路径引用资源（/assets/xxx.js、/vite.svg），
	// 因此不能把资源挂在 /static 前缀下。这里采用"文件优先 + SPA 回退"策略：
	//   1. 静态资源请求（/assets/*、/vite.svg 等带扩展名路径）→ 从 ./static 读取
	//   2. 非 API 的其他路径 → 回退 index.html（交给前端 hash 路由）
	//   3. API 路径未命中 → 404 JSON
	r.NoRoute(func(ctx context.Context, c *app.RequestContext) {
		path := string(c.Request.URI().Path())
		if isAPIPath(path) {
			c.JSON(consts.StatusNotFound, map[string]interface{}{
				"code":    404,
				"message": "API endpoint not found",
			})
			return
		}
		// 静态资源：尝试从 ./static 下读取（去掉前导 /）
		if tryServeStatic(c, path) {
			return
		}
		// 其余路径回退到 SPA index.html。
		// 必须显式 no-cache：无缓存头时浏览器会对 index.html 启发式缓存，
		// 升级后用户停留在旧 bundle 上（hashed assets 引用错位，表现为新功能
		// 不出现/页面错乱），须强刷才能恢复
		c.Header("Cache-Control", "no-cache")
		c.File(filepath.Join(staticOpts.StaticDir, "index.html"))
	})
}

// apiPathPrefixes 仅含"纯 API"前缀——这些前缀下不存在前端页面，
// 未命中路由时返回 JSON 404，而不是回退到 SPA index.html。
//
// 注意：/user、/admin、/anonymous、/share 同时是后端 API 前缀与前端 hash
// 路由的页面路径（如 /user/login、/admin/dashboard、/share/:code）。前端使用
// createWebHashHistory，浏览器实际只请求 "/"，但若用户直接访问这些 history
// 路径（如刷新、外链），应回退到 SPA 由前端路由处理，而非返回 API 404。
// 因此它们不在本列表中。
var apiPathPrefixes = []string{
	"/api", "/chunk", "/notifies",
	"/health", "/live", "/ready", "/readyz", "/ping", "/version",
	"/openapi", "/metrics", "/preview", "/qrcode", "/setup",
}

// isAPIPath 判断路径是否属于后端 API（而非前端 SPA 路由）。
func isAPIPath(path string) bool {
	for _, p := range apiPathPrefixes {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// staticFileExtensions 视为静态资源（而非 SPA 路由）的文件扩展名。
// Vite 构建产物（js/css/图片/字体等）走这里直接返回文件。
var staticFileExtensions = map[string]bool{
	".js": true, ".mjs": true, ".css": true,
	".html": true, ".svg": true, ".png": true, ".jpg": true, ".jpeg": true,
	".gif": true, ".ico": true, ".webp": true, ".woff": true, ".woff2": true,
	".ttf": true, ".eot": true, ".map": true, ".json": true, ".txt": true,
}

// tryServeStatic 尝试从 ./static 目录服务静态资源。
// 命中（文件存在且是静态资源扩展名）时写入响应并返回 true，否则返回 false。
// 用于在 NoRoute 中优先服务 Vite 构建产物（/assets/xxx.js 等），再回退 SPA。
func tryServeStatic(c *app.RequestContext, path string) bool {
	// 仅对带静态资源扩展名的路径尝试（避免目录穿越和无谓的文件系统查找）
	ext := strings.ToLower(filepath.Ext(path))
	if !staticFileExtensions[ext] {
		return false
	}
	// 去掉前导 /，拼接到 static 根目录；filepath.Join 会清理 ../ 等穿越
	rel := strings.TrimPrefix(path, "/")
	fullPath := filepath.Join(staticOpts.StaticDir, rel)
	info, err := os.Stat(fullPath)
	if err != nil || info.IsDir() {
		return false
	}
	c.File(fullPath)
	return true
}
