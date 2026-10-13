package bootstrap

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/gen/router"
)

// TestRegisteredRoutesMatchContract 契约守卫（学习上游 API 契约守卫测试）：
// 以"期望路由表"为契约快照，同时做双向守卫——
//  1. 契约内的每条路由必须已注册（防"IDL 有、路由漏"）；
//  2. 实际注册的路由不得超出契约 ∪ 已知白名单（防手写路由静默漂移，
//     尤其防未受保护的管理端点被顺手加进来）。
//
// 路由有意的增删必须同步修改本文件的 expected 表并过 CI 评审。
func TestRegisteredRoutesMatchContract(t *testing.T) {
	// 最小全局配置：customizedRegister 注册阶段读 Observability/Security 开关
	// （bootstrap 包用自身 config 包级变量，BootstrapWithOptions 才赋值）
	prev := config
	config = &conf.AppConfiguration{
		MCP:      conf.MCPConfig{Enabled: true},
		Security: conf.SecurityConfig{OIDC: conf.OIDCConfig{Enabled: true}},
		UI:       conf.UIConfig{ExposeOpenAPI: true}, // openapi.json 注册受 ui.expose_openapi 门控
	}
	defer func() { config = prev }()

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	router.GeneratedRegister(h)
	customizedRegister(h)

	actual := map[string]bool{}
	for _, r := range h.Routes() {
		actual[r.Method+" "+r.Path] = true
	}

	// expected：IDL 生成路由 + customizedRegister 手写路由的完整契约
	expected := []string{
		// ===== admin（IDL）=====
		"GET /admin/config", "PUT /admin/config",
		// 用户配置运行时读写（IDL，2026-10-10 收编；e3f16f2 管理端用户配置接通运行时）
		"GET /admin/config/user", "PUT /admin/config/user",
		"GET /admin/files", "DELETE /admin/files/:id",
		"POST /admin/login", "GET /admin/stats",
		"GET /admin/users", "PUT /admin/users/:id/status",
		// ===== admin 增强（均受 AdminMiddleware 保护）：用户/文件/统计/日志 2026-10-10 IDL 化，local-files/smtp/oidc/activities 手写 =====
		"GET /admin/activities",
		// 本地文件管理（对标上游 2.7.0 data/local 管理）
		"GET /admin/local-files", "DELETE /admin/local-files", "POST /admin/local-files/import",
		// 设置测试端点
		"POST /admin/notify/smtp/test", "POST /admin/oidc/test",
		"POST /admin/users", "PUT /admin/users/:id", "DELETE /admin/users/:id",
		"POST /admin/users/:id/reset-password", "GET /admin/users/filter",
		"GET /admin/files/:id", "PUT /admin/files/:id",
		"GET /admin/files/:id/download",
		"POST /admin/files/batch-delete", "POST /admin/files/batch-extend",
		// 回收站（2026-10-06）：软删恢复 / 彻底删除（DB 硬删+存储对象删除）
		"POST /admin/files/restore", "POST /admin/files/purge",
		// 站级公告公开端点（2026-10-07 对标上游站内通知条；匿名可访问）
		"GET /api/v1/notifies/public",
		// 分享治理（组合过滤 + 状态机）
		"GET /admin/files/filter",
		"PUT /admin/files/:id/status", "POST /admin/files/batch-status",
		"GET /admin/stats/enhanced", "GET /admin/stats/trend",
		"GET /admin/logs/transfer",
		// ===== maintenance（IDL）=====
		"POST /admin/maintenance/clean-expired", "POST /admin/maintenance/clean-temp",
		"GET /admin/maintenance/logs", "GET /admin/maintenance/system-info",
		"GET /admin/maintenance/monitor/storage", "POST /admin/maintenance/optimize",
		// ===== ratelimit（IDL）=====
		"GET /admin/ratelimit/config", "PUT /admin/ratelimit/config",
		"GET /admin/ratelimit/status", "POST /admin/ratelimit/test",
		// ===== p2p 设备直传（IDL，P2a 2026-10-14）=====
		"GET /admin/p2p/status", "GET /admin/p2p/tasks",
		"POST /admin/p2p/send", "POST /admin/p2p/recv",
		"POST /admin/p2p/cancel", "POST /admin/p2p/publish",
		// ===== storage（IDL）=====
		"GET /admin/storage", "PUT /admin/storage/config",
		"POST /admin/storage/switch", "GET /admin/storage/test/:type",
		"GET /admin/storage/insights", "POST /admin/storage/clean-presign-orphans",
		// ===== notify（IDL）=====
		"GET /admin/notifies", "POST /admin/notifies",
		"GET /admin/notifies/:id", "PUT /admin/notifies/:id", "DELETE /admin/notifies/:id",
		"GET /notifies/active",
		// ===== user（IDL）=====
		"GET /user/api-keys", "DELETE /user/api-keys/:id", "POST /user/api-keys",
		// 一键吊销全部 API Key（手写，JWT-only）
		"POST /user/api-keys/revoke-all",
		"POST /user/change-password", "GET /user/files", "GET /user/info",
		"POST /user/login", "PUT /user/profile", "POST /user/register", "GET /user/stats",
		// ===== share（IDL）=====
		"GET /share/download", "POST /share/file/", "GET /share/select/", "POST /share/text/",
		// 取件元数据（手写：查询不扣次数、不要密码）
		"GET /share/metadata/:code",
		// ===== anonymous（IDL）=====
		"POST /anonymous/generate", "POST /anonymous/retrieve",
		"GET /anonymous/download/:code", "GET /anonymous/search/:code",
		// ===== chunk（IDL）=====
		"POST /chunk/upload/init/",
		"POST /chunk/upload/chunk/:upload_id/:chunk_index",
		"POST /chunk/upload/complete/:upload_id",
		"DELETE /chunk/upload/cancel/:upload_id",
		"GET /chunk/upload/status/:upload_id",
		// ===== presign（IDL + 手写直传端点）=====
		"POST /api/v1/presign/upload", "POST /api/v1/presign/complete", "POST /api/v1/presign/abort",
		"PUT /api/v1/presign/upload-direct/:uploadID",
		// ===== preview / qrcode / setup / health / common（IDL）=====
		"GET /preview/:code",
		"POST /qrcode/generate", "GET /qrcode/:id",
		"POST /setup", "GET /setup/check",
		"GET /health", "GET /live", "GET /ping", "GET /ready", "GET /version",
		"GET /api/v1/ping",
		// ===== customizedRegister 手写 =====
		"GET /openapi.json", "GET /api/config", "GET /robots.txt",
		"POST /api/v1/mcp",
		"POST /api/v1/user/refresh", "POST /api/v1/user/logout", "GET /api/v1/user/check-auth",
		"GET /readyz",
		"GET /api/v1/user/shares", "POST /api/v1/user/shares/batch-delete",
		"POST /api/v1/user/shares/batch-extend", "POST /api/v1/user/shares/:code/restore",
		"DELETE /api/v1/user/shares/:code/hard",
		"GET /api/v1/notifies/mine", "GET /api/v1/notifies/unread-count",
		"POST /api/v1/notifies/mark-read",
		// ===== 多文件分享（P0 多文件，手写；OptionalIdentity 可选身份）=====
		"POST /api/v1/share/multi-direct", "POST /api/v1/share/multi-bind",
		// ===== P2P 联邦解析代理（M2 手写；未启用时 available:false）=====
		"GET /api/v1/federation/resolve",
		// ===== 寄件码/反向收件（P2 手写）=====
		"POST /api/v1/user/requests", "GET /api/v1/user/requests",
		"DELETE /api/v1/user/requests/:token",
		"GET /request/:token", "POST /api/v1/request/:token/upload",
		// ===== OIDC SSO（P2 手写；security.oidc.enabled 时注册）=====
		"GET /api/v1/user/oidc/login", "GET /api/v1/user/oidc/callback",
		// ===== NAS 本地文件导入（P3 手写）=====
		"POST /api/v1/user/shares/import-local",
	}

	// 1. 契约内路由必须全部已注册
	missing := []string{}
	for _, r := range expected {
		if !actual[r] {
			missing = append(missing, r)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("契约路由缺失 %d 条（IDL 声明了但未注册，或手写路由被误删）:\n  %s",
			len(missing), joinLines(missing))
	}

	// 2. 实际路由不得超出契约 ∪ 白名单（静态资源等框架自动注册项）
	allowedExtra := map[string]bool{
		"GET /assets/*filepath":  true, // StaticFS 前端资源
		"HEAD /assets/*filepath": true,
	}
	unexpected := []string{}
	for r := range actual {
		if allowedExtra[r] {
			continue
		}
		found := false
		for _, e := range expected {
			if e == r {
				found = true
				break
			}
		}
		if !found {
			unexpected = append(unexpected, r)
		}
	}
	if len(unexpected) > 0 {
		t.Fatalf("发现契约外路由 %d 条（新路由须同步更新本测试的契约表）:\n  %s",
			len(unexpected), joinLines(unexpected))
	}
}

func joinLines(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += "\n  "
		}
		out += fmt.Sprintf("- %s", s)
	}
	return out
}

// TestDeploymentModes 部署模式守卫（多副本拆分，2026-10-06）：
//  1. public 副本：公开路由在场；管理面路由不得注册（未按模式注册的组），
//     混合生成组中已注册的管理路由（notify 的 /admin/notifies、health 的
//     /version）必须全部命中门卫清单 isAdminPlanePath（防清单漂移漏拦）；
//  2. admin 实例：管理路由在场；公开面路由不得注册；
//  3. standalone 契约由 TestRegisteredRoutesMatchContract 单独守卫。
func TestDeploymentModes(t *testing.T) {
	base := conf.AppConfiguration{
		MCP:      conf.MCPConfig{Enabled: true},
		Security: conf.SecurityConfig{OIDC: conf.OIDCConfig{Enabled: true}},
		UI:       conf.UIConfig{ExposeOpenAPI: true},
	}

	build := func(mode string) map[string]bool {
		prev := config
		cfg := base
		cfg.Deployment.Mode = mode
		config = &cfg
		defer func() { config = prev }()

		h := server.New(server.WithHostPorts("127.0.0.1:0"))
		registerGeneratedRoutes(h)
		customizedRegister(h)
		out := map[string]bool{}
		for _, r := range h.Routes() {
			out[r.Method+" "+r.Path] = true
		}
		return out
	}

	t.Run("public_replica", func(t *testing.T) {
		actual := build(conf.DeployModePublic)

		// 公开面关键路由必须在场
		for _, r := range []string{
			"POST /share/text/", "POST /chunk/upload/init/",
			"POST /anonymous/generate", "GET /notifies/active",
			"POST /user/login", "GET /share/metadata/:code",
			"POST /api/v1/share/multi-direct", "GET /api/config", "GET /ping",
		} {
			if !actual[r] {
				t.Errorf("public 副本缺公开路由: %s", r)
			}
		}

		// 管理面路由：未注册的组必须缺席；注册了的（混合组）必须被门卫覆盖
		registeredAdminPlane := []string{}
		for r := range actual {
			path := strings.TrimPrefix(r, strings.SplitN(r, " ", 2)[0]+" ")
			if isAdminPlanePath(path) {
				registeredAdminPlane = append(registeredAdminPlane, r)
			}
		}
		for _, r := range []string{"GET /admin/stats", "POST /api/v1/mcp", "GET /setup", "POST /setup"} {
			if actual[r] {
				t.Errorf("public 副本不应注册管理路由: %s", r)
			}
		}
		// 混合组残留的 /admin/notifies、/version：必须在门卫清单内（由上面的
		// isAdminPlanePath 收集逻辑保证——若清单漏了它们，registeredAdminPlane
		// 不会包含，本断言即失败）
		if len(registeredAdminPlane) == 0 {
			t.Fatalf("public 副本未检测到任何混合组管理路由（notify/health 组未按预期注册？）: %v", registeredAdminPlane)
		}
		for _, r := range registeredAdminPlane {
			if !strings.HasPrefix(r, "GET /admin/notifies") && r != "GET /version" && !strings.HasPrefix(r, "POST /admin/notifies") &&
				!strings.HasPrefix(r, "PUT /admin/notifies") && !strings.HasPrefix(r, "DELETE /admin/notifies") {
				t.Errorf("public 副本注册了预期外的管理面路由（门卫清单须覆盖）: %s", r)
			}
		}
	})

	t.Run("admin_replica", func(t *testing.T) {
		actual := build(conf.DeployModeAdmin)

		for _, r := range []string{
			"GET /admin/stats", "POST /api/v1/mcp", "GET /admin/notifies",
			"GET /api/config", "GET /ping", "POST /api/v1/user/refresh",
		} {
			if !actual[r] {
				t.Errorf("admin 实例缺管理/支撑路由: %s", r)
			}
		}
		for _, r := range []string{
			"POST /share/text/", "POST /chunk/upload/init/",
			"POST /anonymous/generate", "GET /share/metadata/:code",
			"POST /api/v1/share/multi-direct", "GET /request/:token",
		} {
			if actual[r] {
				t.Errorf("admin 实例不应注册公开面路由: %s", r)
			}
		}
	})
}

// TestStripAdminPlanePaths public 模式 OpenAPI 剥离：管理面路径移除、公开路径保留。
func TestStripAdminPlanePaths(t *testing.T) {
	spec := []byte(`{"openapi":"3.0.0","paths":{"/share/text/":{},"/admin/config":{},"/admin/storage/switch":{},"/version":{},"/api/v1/mcp":{},"/setup":{}}}`)
	out := string(stripAdminPlanePaths(spec))
	for _, banned := range []string{"/admin/config", "/admin/storage/switch", "/version", "/api/v1/mcp", "/setup"} {
		if strings.Contains(out, `"`+banned+`"`) {
			t.Errorf("剥离失败，残留管理面路径 %s: %s", banned, out)
		}
	}
	if !strings.Contains(out, `"/share/text/"`) {
		t.Errorf("公开路径被误删: %s", out)
	}
}
