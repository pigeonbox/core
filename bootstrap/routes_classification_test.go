package bootstrap

import (
	"sort"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/gen/router"
)

// 路由分类守卫（2026-10-26 审计）
//
// ## 要解决的结构性缺口
//
// AGENTS.md 的规则是「**非 admin 面路由全收敛 IDL**」，但**没有任何门禁**
// 能发现漂移：
//
//  1. `TestRegisteredRoutesMatchContract` 的 expected 表把「IDL 生成的路由」
//     与「手写路由」**混在一张扁平表里**，注释用「（手写）」标了一部分；
//     ⇒ 只要把新路由写进这张表，它就合法了 ⇒ **契约层被绕过却无人知情**。
//  2. 实际后果（2026-10-26 实测）：13 条**业务**路由从未进过 contracts IDL
//     （multi-direct / multi-bind / share/metadata / user refresh·logout·check-auth
//     / notifies-public / revoke-all / federation-resolve / import-local /
//     presign upload-direct），前端因此**手写**这些端点的 wire 类型
//     （frontend-core/src/api/{multifile,public,user,notify,federation}.ts）
//     ⇒ 后端改字段名不会编译报错，只在运行时炸。
//
// ## 本守卫的判据
//
// 把「路由属于 IDL 还是手写」从**人的记忆**变成**显式契约 + 机械核对**：
//
//	① 手写路由（真实注册得出）**每一条**都必须在 `handwrittenRouteExemptions`
//	   登记**非空理由** —— 新增手写路由忘登记 ⇒ 变红；
//	② 豁免登记里的**每一条**都必须真实存在（防陈旧条目）且理由非空；
//	③ 待收敛清单（`pendingIDLConvergence`）是**冻结的债务清单**：新增业务手写
//	   路由必须二选一 —— ①收敛进 IDL，或 ②在豁免表里显式登记理由；
//	④ IDL 生成的路由集与手写路由集**互斥且完备**（两边都注册一次取真实集合）。
//
// ## 负对照
//
//	· 新增一条手写路由而不登记豁免  ⇒ ① 红；
//	· 从豁免表删掉一条已存在的路由    ⇒ ② 红；
//	· 把某条理由清空                  ⇒ ② 红；
//	· 把一条 pending 从待收敛清单删掉  ⇒ ③ 红（该路由必须落在两类之一）。

// handwrittenExemption 一条手写路由的豁免登记。
type handwrittenExemption struct {
	// Reason 为什么它不在 contracts IDL 里（**必填**，非空）。
	Reason string
	// Pending true = 待收敛进 IDL 的**业务债务**；
	// false = 永久豁免（平台面 / 协议面 / 非 JSON 包络，IDL 模型表达不了）。
	Pending bool
}

// handwrittenRouteExemptions 手写路由的**完整**豁免清单。
//
// 键的格式为 "<METHOD> <PATH>"（与 hertz 的路由描述一致）。
//
// ⚠️ 新增手写路由时**必须**在此登记并写明理由 —— 否则本守卫变红。
// 若该路由是业务 API，正确做法是**收敛进 contracts IDL**（Pending 债务清单
// 正是为此存在）；只有平台面/协议面/非 JSON 包络才做永久豁免。
var handwrittenRouteExemptions = map[string]handwrittenExemption{
	// ===== 业务债务：待收敛进 IDL（wire 形状目前由前端手写）=====
	"POST /api/v1/share/multi-direct": {
		Reason:  "P0 多文件分享的直传入口；multipart 流式上传 + 服务端合并，当前 wire 形状由前端手写（frontend-core/src/api/multifile.ts）⇒ 后端改字段名不会编译报错",
		Pending: true,
	},
	"POST /api/v1/share/multi-bind": {
		Reason:  "P0 多文件分享的绑定入口（分片通道合并）；当前 wire 形状由前端手写（frontend-core/src/api/multifile.ts）",
		Pending: true,
	},
	"GET /share/metadata/:code": {
		Reason:  "取件元信息（**交付面**：回显文件名/大小）⇒ 交付形状必须受契约保护，否则前端与后端的展示字段会各写各的",
		Pending: true,
	},
	"POST /api/v1/user/refresh": {
		Reason:  "会话令牌刷新（前端 401 拦截器调用）；返回体含新 token 与轮换 Cookie 语义，契约层尚无对应服务",
		Pending: true,
	},
	"POST /api/v1/user/logout": {
		Reason:  "会话登出（清 Cookie）；契约层尚无对应服务",
		Pending: true,
	},
	"GET /api/v1/user/check-auth": {
		Reason:  "前端启动时的登录态探测；契约层尚无对应服务",
		Pending: true,
	},
	"GET /api/v1/notifies/public": {
		Reason:  "站级公告公开端点（匿名可访问）；契约层尚无对应服务",
		Pending: true,
	},
	"POST /user/api-keys/revoke-all": {
		Reason:  "一键吊销全部 API Key；契约层只有单条吊销（DELETE /user/api-keys/:id）",
		Pending: true,
	},
	"GET /api/v1/federation/resolve": {
		Reason:  "联邦实例解析代理（未启用时返回 available:false）；契约层尚无对应服务",
		Pending: true,
	},
	"POST /api/v1/user/shares/import-local": {
		Reason:  "NAS 本地文件免上传导入（P3）；契约层只有管理台变体（AdminImportLocalFile），用户面尚无",
		Pending: true,
	},

	// ===== 永久豁免：平台面 / 协议面 / 非 JSON 包络 =====
	"GET /openapi.json": {
		Reason:  "平台面：OpenAPI 文档端点本身（契约层的**产物**，不是契约层的一部分）；且受 ui.expose_openapi 开关门控（默认关闭=不注册）",
		Pending: false,
	},
	"GET /api/config": {
		Reason:  "平台面：公开运行时配置（前端启动引导用），非业务 API",
		Pending: false,
	},
	"GET /robots.txt": {
		Reason:  "平台面：静态文本",
		Pending: false,
	},
	"GET /readyz": {
		Reason:  "平台面：K8s 就绪探针（IDL 的 /ready 是业务健康面，两者并存：/ready 在 IDL 里、/readyz 是容器编排探针）",
		Pending: false,
	},
	"POST /api/v1/mcp": {
		Reason:  "协议面：MCP（Streamable HTTP）JSON-RPC 流，**不是** REST/JSON 包络 ⇒ thrift IDL 模型表达不了",
		Pending: false,
	},
	"GET /api/v1/mcp": {
		Reason:  "协议面：MCP（Streamable HTTP）按规范回 405，由 SDK 应答",
		Pending: false,
	},
	"DELETE /api/v1/mcp": {
		Reason:  "协议面：MCP（Streamable HTTP）按规范回 405，由 SDK 应答",
		Pending: false,
	},
	"GET /api/v1/user/oidc/login": {
		Reason:  "协议面：OIDC 登录入口返回 **302 重定向**到 IdP，非 JSON 包络 ⇒ IDL 表达不了",
		Pending: false,
	},
	"GET /api/v1/user/oidc/callback": {
		Reason:  "协议面：OIDC 回调返回 **302 重定向**（或错误页），非 JSON 包络 ⇒ IDL 表达不了",
		Pending: false,
	},
	"PUT /api/v1/presign/upload-direct/:uploadID": {
		Reason:  "协议面：预签名直传的**写对象**端点，body 是原始对象字节流（非 JSON 包络）⇒ IDL 表达不了",
		Pending: false,
	},
	"GET /assets/*filepath": {
		Reason:  "平台面：SPA 前端静态资源（StaticFS 挂载）；由框架按文件路径通配，不属业务 API",
		Pending: false,
	},
	"HEAD /assets/*filepath": {
		Reason:  "平台面：SPA 前端静态资源的 HEAD 探测（框架自动注册）；不属业务 API",
		Pending: false,
	},
}

// TestHandwrittenRoutesAreClassified 路由分类守卫（见文件头注释）。
func TestHandwrittenRoutesAreClassified(t *testing.T) {
	prev := config
	config = &conf.AppConfiguration{
		MCP:      conf.MCPConfig{Enabled: true},
		Security: conf.SecurityConfig{OIDC: conf.OIDCConfig{Enabled: true}},
		UI:       conf.UIConfig{ExposeOpenAPI: true},
	}
	defer func() { config = prev }()

	// ① 真实注册集：IDL 生成路由（单独注册一次）与手写路由（单独注册一次）
	idlRoutes := map[string]bool{}
	h1 := server.New(server.WithHostPorts("127.0.0.1:0"))
	router.GeneratedRegister(h1)
	for _, r := range h1.Routes() {
		idlRoutes[r.Method+" "+r.Path] = true
	}

	handRoutes := map[string]bool{}
	h2 := server.New(server.WithHostPorts("127.0.0.1:0"))
	customizedRegister(h2)
	for _, r := range h2.Routes() {
		handRoutes[r.Method+" "+r.Path] = true
	}

	if len(idlRoutes) == 0 {
		t.Fatal("IDL 生成路由集为空 —— 守卫自身失效（生成路由没注册上）")
	}

	// ② 手写路由每条都要登记理由
	var unregistered []string
	for r := range handRoutes {
		if _, ok := handwrittenRouteExemptions[r]; !ok {
			unregistered = append(unregistered, r)
		}
	}
	if len(unregistered) > 0 {
		sort.Strings(unregistered)
		t.Errorf("新增了 %d 条**手写**路由但未在 handwrittenRouteExemptions 登记理由（业务 API 应先收敛进 contracts IDL；平台/协议面才做永久豁免）:\n  %s",
			len(unregistered), joinLines(unregistered))
	}

	// ③ 豁免登记必须真实存在 + 理由非空（防陈旧条目 / 空头承诺）
	for r, ex := range handwrittenRouteExemptions {
		if !handRoutes[r] {
			t.Errorf("豁免登记的路由 %q 在真实注册表里不存在（陈旧条目）", r)
		}
		if strings.TrimSpace(ex.Reason) == "" {
			t.Errorf("路由 %q 的豁免理由为空（空头承诺：必须写清为什么它不在 IDL 里）", r)
		}
	}

	// ④ 待收敛清单必须与豁免表一致（pending 标记与「待收敛」集合同源）
	for r, ex := range handwrittenRouteExemptions {
		if !ex.Pending {
			continue
		}
		if !handRoutes[r] {
			t.Errorf("待收敛清单里的路由 %q 未注册（陈旧债务条目）", r)
		}
	}

	// ⑤ 两类必须互斥完备：手写路由集合 == 豁免表键集合（登记过、且都真实存在）
	if len(handRoutes) != len(handwrittenRouteExemptions) {
		t.Logf("提示：手写路由 %d 条 vs 豁免登记 %d 条（差值应在上文报错里逐条列出）",
			len(handRoutes), len(handwrittenRouteExemptions))
	}

	// ⑥ IDL 生成集与手写集不得有交集（同一 method+path 注册两次 ⇒ Hertz panic，
	//    这里提前给出可读诊断）
	for r := range idlRoutes {
		if handRoutes[r] {
			t.Errorf("路由 %q 同时出现在 IDL 生成集与手写集里（重复注册 ⇒ Hertz 启动 panic）", r)
		}
	}
}
