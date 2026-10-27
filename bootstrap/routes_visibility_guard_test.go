package bootstrap

// 10 条 IDL 化业务路由的**部署面可见性**机械守卫（2026-10-27）。
//
// ## 为什么要有这份守卫
//
// 这 10 条路由收敛进 IDL 之前是**手写注册**，注册条件写在
// `bootstrap/routes_custom.go` 里（两种条件混在一处）：
//
//	· 写在 `if config.ServesPublicPlane(){}` 内 ⇒ **仅公开副本**有
//	  （share/metadata、share/multi-direct、share/multi-bind、
//	    user/shares/import-local、federation/resolve）
//	· 无条件注册 ⇒ **公开副本与管理副本都有**
//	  （user/refresh、user/logout、user/check-auth、
//	    user/api-keys/revoke-all、notifies/public）
//
// 收敛进 IDL 后，路由改由 hz 生成 ⇒ **一个 IDL 域 = 一个生成组 =
// 整组注册/不注册**（见 bootstrap/deployment.go 的 registerGeneratedRoutes）
// ⇒ 域归属**直接决定**路由在哪些副本上存在。
//
// ⚠️ 这条不变性（每条路由的副本可见性与收敛前逐条一致）是**行为红线**：
// 域被顺手挪走 ⇒ 某条路由在 admin 副本上凭空出现（如多文件上传面被放大到
// 管理副本）或凭空消失（如管理控制台刷不了令牌 / 联邦解析在两个副本上都没了）。
// 而"挪域"在 hz 生成链里只是一行 IDL 改动 ⇒ **编译不报错、CI 不一定红**。
//
// ## 本守卫的判据
//
// 把"每条路由该在哪些副本上"写成**一张显式表**（与 TestDeploymentModes 同风格），
// 逐条对照**真实注册路由表**（h.Routes()，三种部署模式各注册一次）：
//
//	· 该在的缺席 ⇒ 红（路由被挪到别的域 / 生成组没被注册）
//	· 不该在的在场 ⇒ 红（部署面被放大）
//
// ## 自动进核对（新增路由不用改本文件）
//
// 期望表的**键集合**必须与 `idlizedRoutes`（router_auth_guard_test.go 里
// 「本轮 IDL 化面契约」的路由清单）**完全一致** ⇒ 往那份清单里加一条新路由
// 而忘了在本表登记它的可见性 ⇒ 本守卫变红 ⇒ 新路由必须显式声明副本可见性。
//
// ## 负对照（必须能变红；已实跑）
//
//	· 把 bootstrap/deployment.go 的 admin 分支加上 genpublicapi.Register(h)
//	  ⇒ admin 副本凭空多出 5 条公开面路由 ⇒ 本守卫变红。
//	· 把 bootstrap/deployment.go 的 public 分支去掉 gencommon.Register(h)
//	  ⇒ public 副本少掉 5 条两面路由 ⇒ 本守卫变红。

import (
	"sort"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"

	"github.com/pigeonbox/core/conf"
)

// routeVisibility 一条路由在三种部署模式下的注册期望。
type routeVisibility struct {
	standalone bool
	public     bool
	admin      bool
}

// idlizedRouteVisibility 10 条 IDL 化路由的**副本可见性**期望表。
//
// 口径与收敛前 routes_custom.go 的手写注册条件**逐条一致**：
//
//	· 仅公开副本（standalone + public，**无 admin**）——收敛前写在
//	  `if config.ServesPublicPlane(){}` 内的那 5 条；
//	· 两面都有（standalone + public + admin）——收敛前无条件注册的那 5 条。
//
// ⚠️ 这张表是**显式意图**，不是从域名反推的：域归属由
// router_auth_guard_test.go 的 TestIdlizedRouteContract 单独钉，
// 本表钉的是「这条路由在哪些副本上存在」——两者是**两条独立的红线**。
var idlizedRouteVisibility = map[string]routeVisibility{
	// ===== 仅公开副本（收敛前写在 `if config.ServesPublicPlane(){}` 内）=====
	"GET /share/metadata/:code":             {standalone: true, public: true, admin: false},
	"POST /api/v1/share/multi-direct":       {standalone: true, public: true, admin: false},
	"POST /api/v1/share/multi-bind":         {standalone: true, public: true, admin: false},
	"POST /api/v1/user/shares/import-local": {standalone: true, public: true, admin: false},
	"GET /api/v1/federation/resolve":        {standalone: true, public: true, admin: false},

	// ===== 两面都有（收敛前是无条件注册）=====
	"POST /api/v1/user/refresh":      {standalone: true, public: true, admin: true},
	"POST /api/v1/user/logout":       {standalone: true, public: true, admin: true},
	"GET /api/v1/user/check-auth":    {standalone: true, public: true, admin: true},
	"POST /user/api-keys/revoke-all": {standalone: true, public: true, admin: true},
	"GET /api/v1/notifies/public":    {standalone: true, public: true, admin: true},
}

// TestIdlizedRouteDeploymentVisibility 逐条核对 10 条 IDL 化路由的副本可见性。
//
// 注册来源与生产一致：registerGeneratedRoutes（按部署模式挑生成组）+
// customizedRegister（手写路由）——两边都注册后再取 h.Routes()。
func TestIdlizedRouteDeploymentVisibility(t *testing.T) {
	// ① 期望表与「本轮 IDL 化面契约」的路由清单必须**完全一致**（自动进核对）：
	//    往 idlizedRoutes 加了新路由而忘了在本表登记可见性 ⇒ 变红。
	want := make([]string, 0, len(idlizedRoutes))
	for k := range idlizedRoutes {
		want = append(want, k)
	}
	got := make([]string, 0, len(idlizedRouteVisibility))
	for k := range idlizedRouteVisibility {
		got = append(got, k)
	}
	sort.Strings(want)
	sort.Strings(got)
	if len(want) != len(got) {
		t.Fatalf("可见性期望表与 IDL 化路由清单不一致：清单 %d 条、期望表 %d 条\n  清单: %v\n  期望表: %v",
			len(want), len(got), want, got)
	}
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("可见性期望表与 IDL 化路由清单不一致：清单 %q、期望表 %q", want[i], got[i])
		}
	}

	base := conf.AppConfiguration{
		MCP:      conf.MCPConfig{Enabled: true},
		Security: conf.SecurityConfig{OIDC: conf.OIDCConfig{Enabled: true}},
		UI:       conf.UIConfig{ExposeOpenAPI: true},
	}

	// 按模式取真实注册路由集。
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

	modes := []struct {
		name  string
		mode  string
		want  func(v routeVisibility) bool
		label string
	}{
		{"standalone", conf.DeployModeStandalone, func(v routeVisibility) bool { return v.standalone }, "单进程（standalone）"},
		{"public", conf.DeployModePublic, func(v routeVisibility) bool { return v.public }, "公开副本（public）"},
		{"admin", conf.DeployModeAdmin, func(v routeVisibility) bool { return v.admin }, "管理副本（admin）"},
	}

	for _, m := range modes {
		m := m
		t.Run(m.name, func(t *testing.T) {
			actual := build(m.mode)
			for route, v := range idlizedRouteVisibility {
				got := actual[route]
				want := m.want(v)
				if got != want {
					if want {
						t.Errorf("%s 缺路由 %s（该在却不在）—— 域被挪走或生成组没被注册 ⇒ 副本可见性变了",
							m.label, route)
					} else {
						t.Errorf("%s 多出路由 %s（不该在却在了）—— 部署面被放大（该路由收敛前只存在于公开面）",
							m.label, route)
					}
				}
			}
		})
	}
}
