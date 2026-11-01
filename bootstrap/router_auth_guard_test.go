package bootstrap

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/gen/router"
	"github.com/pigeonbox/core/pkg/middleware"
)

// 本文件是「生成路由的鉴权接线」机械守卫。
//
// 背景（2026-10-26，10 条业务路由 IDL 化）：hz 生成的路由，中间件链默认是空的
// —— gen/router/<domain>/middleware.go 里每个 _xxxMw() 初始就是 `return nil`。
// 手写注册时代鉴权写在 customHandler 里；一旦改成生成路由而忘了在 middleware.go
// 补鉴权，这条路由就变成**免鉴权**，且没有任何编译期/测试期信号（revoke-all
// 这类「一票否决」端点尤其致命）。
//
// 反过来的坑同样致命：**公开端点被误加登录门槛**。hz 按**路径**建组
// （cmd/hz 的 RouterNode.FindNearest/Insert 走路径树），同一路径前缀下的路由共用
// 同一组中间件 ⇒ 把 /api/v1/notifies/public 收进 notify.thrift（其 /api 组挂了
// UserOrAPIKey）就等于给公开公告端点加了登录门槛（2026-10-26 实录：匿名访问
// 从 200 变 401）。
//
// 守卫由三层组成：
//  1. 静态契约（TestGeneratedRouteAuthContract）
//     · mwContract：每个生效中间件必须登记它提供的鉴权口径（防"换了口径"）；
//     · publicRoutes：必须保持匿名可达的路由必须登记理由（防"被误加鉴权"）；
//     · 覆盖闭合：任何生成路由要么带强制鉴权，要么在 publicRoutes 里；
//       新增路由忘了接线/忘了登记 ⇒ 变红。
//  2. 本轮 IDL 化面契约（TestIdlizedRouteContract）
//     钉死 10 条路由的**所属 IDL 域**（域 = 生成组 = 副本可见性）与鉴权口径。
//  3. 行为级对照（TestGeneratedRouteAuthEnforcedAtRuntime）
//     未带凭据打一次：说"受保护"的必须被挡，说"公开"的不得被鉴权中间件挡。
//
// 解析对象是**生成物本身**（gen/router/*/*.go），不是手抄路由清单：hz 重新生成
// 路由后守卫自动跟着变，不需要人工同步第二份路由表。

// 强制鉴权的中间件构造器（出现在中间件表达式的返回里即视为"这条路由受保护"）。
// 用词边界匹配：OptionalAuthMiddleware 不含独立的 AuthMiddleware 词元，
// 不会被误判成强制鉴权。
var enforcingAuthRe = regexp.MustCompile(
	`\b(AdminMiddleware|AuthMiddleware|AdminOrAdminKey|UserOrAPIKey|UserAuth|RequireAdmin|RequireAnyRole|RequireRole)\b`)

// 可选身份（匿名可达、登录时注入身份）—— 不算强制鉴权。
var optionalAuthRe = regexp.MustCompile(
	`\b(OptionalIdentity|OptionalAuthMiddleware|OptionalAPIKey)\b`)

// enforcingKinds 强制鉴权口径集合（用于把"鉴权口径"分成强制/可选两类）。
var enforcingKinds = map[string]bool{
	"AdminMiddleware": true, "AuthMiddleware": true, "AdminOrAdminKey": true,
	"UserOrAPIKey": true, "UserAuth": true,
	"RequireAdmin": true, "RequireAnyRole": true, "RequireRole": true,
}

// 生成路由的一条路由及其有效中间件链。
type generatedRoute struct {
	domain    string
	method    string
	path      string
	mwNames   []string // 有效链上的中间件函数名（组级 + 路由级，外→内）
	authKinds []string // 有效链上的全部鉴权口径（强制 + 可选）
}

// key 用于表查找与错误信息。
func (r generatedRoute) key() string { return r.method + " " + r.path }

// enforcing 返回有效链上的强制鉴权口径。
func (r generatedRoute) enforcing() []string {
	var out []string
	for _, k := range r.authKinds {
		if enforcingKinds[k] {
			out = append(out, k)
		}
	}
	return out
}

// 生成路由目录（相对本测试文件）。
func generatedRouterDir(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) 失败：无法定位测试文件")
	}
	return filepath.Join(filepath.Dir(self), "..", "gen", "router")
}

// ===== 契约表 =====

// mwContract：每个**生效**（非空）的中间件函数 → 它提供的鉴权口径。
// 键为 "<IDL 域>.<中间件函数名>"。
//
// 登记的意义：中间件被换掉（例如 UserOrAPIKey 被改成 AuthMiddleware ⇒ 用户的
// API Key 突然管不了自己的分享）时守卫会变红，而不是等到线上才发现。
var mwContract = map[string][]string{
	"admin._adminMw":                     {"AdminMiddleware"},
	"chunk._chunkMw":                     {"OptionalIdentity"},
	"common._usercheckauthMw":            {"UserAuth"},
	"common._userapikeysrevokeallMw":     {"AuthMiddleware"},
	"maintenance._adminMw":               {"AdminMiddleware"},
	"notify._adminMw":                    {"AdminMiddleware"},
	"notify._apiMw":                      {"UserOrAPIKey"},
	"p2p._adminMw":                       {"AdminOrAdminKey"},
	"presign._presignMw":                 {"OptionalIdentity"},
	"publicapi._multisharebindMw":        {"OptionalIdentity"},
	"publicapi._multisharedirectMw":      {"OptionalIdentity"},
	"publicapi._usersharesimportlocalMw": {"UserOrAPIKey"},
	"ratelimit._adminMw":                 {"AdminMiddleware"},
	"request._userMw":                    {"UserAuth"},
	"share._apiMw":                       {"UserOrAPIKey"},
	"share._sharefileMw":                 {"OptionalIdentity"},
	"share._selectMw":                    {"OptionalIdentity"},
	"share._sharetextMw":                 {"OptionalIdentity"},
	"storage._adminMw":                   {"AdminMiddleware"},
	"user._changepasswordMw":             {"AuthMiddleware"},
	"user._createapikeyMw":               {"AuthMiddleware"},
	"user._deleteapikeyMw":               {"AuthMiddleware"},
	"user._listapikeysMw":                {"AuthMiddleware"},
	"user._updateprofileMw":              {"AuthMiddleware"},
	"user._userfilesMw":                  {"AuthMiddleware"},
	"user._userinfoMw":                   {"AuthMiddleware"},
	"user._userstatsMw":                  {"AuthMiddleware"},
}

// publicRoutes：必须保持**匿名可达**的生成路由 → 理由。
//
// 登记的意义：
//  1. 防"被误加鉴权"（本轮实测踩中：公开公告端点被罩上 UserOrAPIKey）；
//  2. 防"漏登记"——凡是没登记又没强制鉴权的生成路由，守卫一律判红。
var publicRoutes = map[string]string{
	// —— 探针与版本 ——
	"GET /health":      "容器编排存活/就绪探针",
	"GET /live":        "存活探针（与 /health 同源）",
	"GET /ready":       "就绪探针",
	"GET /ping":        "探针别名（兼容上游）",
	"GET /version":     "版本信息（公开）",
	"GET /api/v1/ping": "Ping 检查",

	// —— 安装 / 登录 ——
	"GET /setup/check":    "初始化状态检查（尚未安装时也必须可达）",
	"POST /setup":         "首次初始化（此时还没有管理员账号）",
	"POST /user/login":    "登录",
	"POST /user/register": "注册",

	// —— 公开内容面 ——
	"GET /notifies/active":            "站级公告条（前端 SiteNotice 组件）",
	"GET /api/v1/notifies/public":     "站级公开公告列表（匿名；本轮从 notify 域挪到 common 域）",
	"GET /qrcode/:id":                 "二维码内容",
	"POST /qrcode/generate":           "生成二维码",
	"GET /preview/:code":              "分享预览",
	"GET /share/metadata/:code":       "取件元数据（查询不扣次数、不要密码）",
	"GET /share/download":             "取件下载（口令/令牌在 handler 内校验）",
	"GET /share/select":               "文本取件（口令/令牌在 handler 内校验）",
	"POST /share/file":                "匿名寄件（文件）",
	"POST /share/text":                "匿名寄件（文本）",
	"POST /api/v1/share/multi-direct": "多文件直传（可选身份：匿名可用）",
	"POST /api/v1/share/multi-bind":   "多文件绑定（可选身份：匿名可用）",
	"GET /api/v1/federation/resolve":  "联邦节点解析（公开）",

	// —— 跨站投递（联邦前台化，2026-11-01；公开面，口令 bearer 在 handler 内校验）——
	"POST /api/v1/federation/delivery-slot": "跨站投递槽位（公开端点；共享口令 bearer 授权）",
	"POST /api/v1/federation/deliver":       "跨站投递（公开端点；目标须在管理员登记的对端名单内）",

	// —— 直传/分片（可选身份：匿名可用）——
	"POST /api/v1/presign/upload":                      "预签名直传",
	"POST /api/v1/presign/complete":                    "预签名直传完成",
	"POST /api/v1/presign/abort":                       "预签名直传中止",
	"POST /chunk/upload/init":                          "分片会话初始化",
	"POST /chunk/upload/chunk/:upload_id/:chunk_index": "分片上传",
	"POST /chunk/upload/complete/:upload_id":           "分片会话完成",
	"DELETE /chunk/upload/cancel/:upload_id":           "分片会话取消",
	"GET /chunk/upload/status/:upload_id":              "分片会话状态",

	// —— 寄件码（访客公开投递）——
	"GET /request/:token":                "寄件码公开查询",
	"POST /api/v1/request/:token/upload": "访客投递",

	// —— 匿名取件 ——
	"POST /anonymous/generate":      "匿名取件码生成",
	"POST /anonymous/retrieve":      "匿名取件凭证（只发凭证不扣次）",
	"GET /anonymous/search/:code":   "匿名取件搜索",
	"GET /anonymous/download/:code": "匿名取件下载",

	// —— 会话（令牌在 handler 内校验，中间件层不设门槛）——
	"POST /api/v1/user/refresh": "会话刷新（handler 内校验令牌；缺令牌回 401）",
	"POST /api/v1/user/logout":  "显式注销（handler 内校验令牌；缺令牌回 401）",

	// —— 联邦推送邀请（公开收件）——
	"POST /api/v1/p2p/invite": "联邦实例间推送邀请（公开收件端点）",
}

// idlizedRouteContract：2026-10-26 本轮 IDL 化的 10 条业务路由 ——
// 钉死「所属 IDL 域」与「有效鉴权口径」。
//
// 域这一维必须钉：core 的部署模式过滤是**按 hz 生成的路由组**挑选
// （bootstrap/deployment.go 的 registerGeneratedRoutes）⇒ 一个 IDL 域 = 一个生成组
// = 整组注册/不注册 ⇒ 域归属直接决定路由在哪些副本上存在。域被顺手挪走 =
// 副本可见性变了（admin 副本上凭空多出/少掉路由）。
type idlizedRouteContract struct {
	domain string
	kinds  []string // 期望的有效鉴权口径（空 = 匿名可达）
}

var idlizedRoutes = map[string]idlizedRouteContract{
	// 仅 public 面（收编前写在 `if config.ServesPublicPlane(){}` 内）
	"GET /api/v1/federation/resolve":        {domain: "publicapi", kinds: nil},
	"GET /share/metadata/:code":             {domain: "publicapi", kinds: nil},
	"POST /api/v1/share/multi-direct":       {domain: "publicapi", kinds: []string{"OptionalIdentity"}},
	"POST /api/v1/share/multi-bind":         {domain: "publicapi", kinds: []string{"OptionalIdentity"}},
	"POST /api/v1/user/shares/import-local": {domain: "publicapi", kinds: []string{"UserOrAPIKey"}},
	// 跨站投递（联邦前台化，2026-11-01）：公开面（口令 bearer 在 handler 内校验
	// ⇒ 中间件层不设门槛，与 /api/v1/p2p/invite 同款）。
	"POST /api/v1/federation/delivery-slot": {domain: "publicapi", kinds: nil},
	"POST /api/v1/federation/deliver":       {domain: "publicapi", kinds: nil},
	// 两面都注册（收编前是无条件注册）
	"POST /api/v1/user/refresh":      {domain: "common", kinds: nil},
	"POST /api/v1/user/logout":       {domain: "common", kinds: nil},
	"GET /api/v1/user/check-auth":    {domain: "common", kinds: []string{"UserAuth"}},
	"POST /user/api-keys/revoke-all": {domain: "common", kinds: []string{"AuthMiddleware"}},
	"GET /api/v1/notifies/public":    {domain: "common", kinds: nil},
}

// ===== 解析层 =====

// parseGeneratedRoutes 解析 gen/router 下所有域的生成路由，产出「路由 + 有效中间件链」。
//
// 有效链 = 沿途每个 Group 的 mw（父→子）+ 路由自身 append(_xxxMw(), handler) 的 mw。
// hertz 的语义就是「Group(path, mw...) 的 mw 对该组及子组全部生效」。
func parseGeneratedRoutes(t *testing.T) []generatedRoute {
	t.Helper()
	dir := generatedRouterDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取生成路由目录失败 %s: %v", dir, err)
	}
	var out []generatedRoute
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		domain := e.Name()
		mws := parseMwFile(t, filepath.Join(dir, domain, "middleware.go"))
		out = append(out, parseRouterFile(t, filepath.Join(dir, domain, domain+".go"), domain, mws)...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].path != out[j].path {
			return out[i].path < out[j].path
		}
		return out[i].method < out[j].method
	})
	return out
}

// mwInfo 是 middleware.go 里一个 _xxxMw() 的静态画像。
type mwInfo struct {
	name string
	// empty=true 表示该中间件函数返回空链（`return nil` 或空切片字面量）。
	empty bool
	// expr 是该函数所有 return 表达式的源码拼接（用于识别鉴权构造器）。
	expr string
}

// parseMwFile 解析 gen/router/<domain>/middleware.go，抽出每个中间件函数的画像。
func parseMwFile(t *testing.T, path string) map[string]mwInfo {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("解析 middleware.go 失败 %s: %v", path, err)
	}
	out := map[string]mwInfo{}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Recv != nil || fd.Body == nil {
			continue
		}
		var exprs []string
		empty := true
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			ret, ok := n.(*ast.ReturnStmt)
			if !ok || len(ret.Results) != 1 {
				return true
			}
			exprs = append(exprs, exprSource(fset, ret.Results[0]))
			if !isEmptyMwExpr(ret.Results[0]) {
				empty = false
			}
			return true
		})
		out[fd.Name.Name] = mwInfo{name: fd.Name.Name, empty: empty, expr: strings.Join(exprs, " ; ")}
	}
	return out
}

// exprSource 打印一个表达式的源码（注释不参与，避免注释里的函数名造成误判）。
func exprSource(fset *token.FileSet, e ast.Expr) string {
	var sb strings.Builder
	if err := printer.Fprint(&sb, fset, e); err != nil {
		return ""
	}
	return sb.String()
}

// isEmptyMwExpr 判断一个 return 表达式是否"没有挂任何中间件"。
func isEmptyMwExpr(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name == "nil"
	case *ast.CompositeLit:
		return len(v.Elts) == 0
	}
	return false
}

// groupInfo 是一个 Group 变量的路径与继承到的中间件链。
type groupInfo struct {
	path  string
	chain []string
}

// parseRouterFile 解析单个域的生成路由文件。
func parseRouterFile(t *testing.T, path, domain string, mws map[string]mwInfo) []generatedRoute {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("解析路由文件失败 %s: %v", path, err)
	}
	groups := map[string]groupInfo{}
	var out []generatedRoute

	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		walkRouterBlock(t, fset, fd.Body, groups, mws, domain, &out)
	}
	return out
}

var httpMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "DELETE": true,
	"PATCH": true, "HEAD": true, "OPTIONS": true, "ANY": true,
}

// walkRouterBlock 递归遍历 hz 生成的嵌套块结构（每层是一个独立 BlockStmt）。
func walkRouterBlock(t *testing.T, fset *token.FileSet, block *ast.BlockStmt,
	groups map[string]groupInfo, mws map[string]mwInfo, domain string, out *[]generatedRoute) {
	t.Helper()
	for _, stmt := range block.List {
		switch s := stmt.(type) {
		case *ast.AssignStmt:
			// x := parent.Group("/seg", _xMw()...)
			if len(s.Rhs) != 1 || len(s.Lhs) != 1 {
				continue
			}
			call, ok := s.Rhs[0].(*ast.CallExpr)
			if !ok {
				continue
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Group" {
				continue
			}
			parentIdent, ok := sel.X.(*ast.Ident)
			if !ok {
				continue
			}
			varName, ok := s.Lhs[0].(*ast.Ident)
			if !ok {
				continue
			}
			parent := groups[parentIdent.Name] // 根 Group（r.Group）不在表里 → 零值
			seg := stringArg(fset, call.Args, 0)
			chain := append([]string{}, parent.chain...)
			if mw := mwArgIdent(call.Args, 1); mw != "" {
				chain = append(chain, mw)
			}
			groups[varName.Name] = groupInfo{path: joinRoutePath(parent.path, seg), chain: chain}

		case *ast.ExprStmt:
			// grp.METHOD("/seg", append(_xMw(), handler)...)
			call, ok := s.X.(*ast.CallExpr)
			if !ok {
				continue
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !httpMethods[sel.Sel.Name] {
				continue
			}
			grpIdent, ok := sel.X.(*ast.Ident)
			if !ok {
				continue
			}
			grp, ok := groups[grpIdent.Name]
			if !ok {
				continue
			}
			seg := stringArg(fset, call.Args, 0)
			chain := append([]string{}, grp.chain...)
			if len(call.Args) >= 2 {
				if mw := mwArgIdent(call.Args, 1); mw != "" {
					chain = append(chain, mw)
				}
			}
			r := generatedRoute{
				domain:  domain,
				method:  sel.Sel.Name,
				path:    joinRoutePath(grp.path, seg),
				mwNames: chain,
			}
			for _, name := range chain {
				info, ok := mws[name]
				if !ok {
					continue
				}
				r.authKinds = appendUnique(r.authKinds, enforcingAuthRe.FindAllString(info.expr, -1)...)
				r.authKinds = appendUnique(r.authKinds, optionalAuthRe.FindAllString(info.expr, -1)...)
			}
			*out = append(*out, r)

		case *ast.BlockStmt:
			walkRouterBlock(t, fset, s, groups, mws, domain, out)
		}
	}
}

// stringArg 取调用参数里第 i 个字符串字面量的值。
func stringArg(fset *token.FileSet, args []ast.Expr, i int) string {
	if i >= len(args) {
		return ""
	}
	lit, ok := args[i].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	return strings.Trim(lit.Value, `"`)
}

// mwArgIdent 从调用参数里认出「中间件函数调用」，返回函数名（没有则空串）。
// 形如 _apiMw()... / append(_apiMw(), handler)
func mwArgIdent(args []ast.Expr, i int) string {
	if i >= len(args) {
		return ""
	}
	call, ok := args[i].(*ast.CallExpr)
	if !ok {
		return ""
	}
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		// append(_xxxMw(), handler) → 取第一个实参（builtin append 也是 Ident，
		// 不特判会把中间件名记成 "append"，路由级鉴权就整体漏判）
		if fun.Name == "append" {
			return mwArgIdent(call.Args, 0)
		}
		return fun.Name
	case *ast.SelectorExpr:
		// append(_xxxMw(), handler) → 取第一个实参
		return mwArgIdent(call.Args, 0)
	}
	return ""
}

func joinRoutePath(parent, seg string) string {
	if seg == "" || seg == "/" {
		if parent == "" {
			return "/"
		}
		return parent
	}
	if parent == "" || parent == "/" {
		return seg
	}
	return parent + seg
}

func appendUnique(dst []string, vals ...string) []string {
	for _, v := range vals {
		found := false
		for _, d := range dst {
			if d == v {
				found = true
				break
			}
		}
		if !found {
			dst = append(dst, v)
		}
	}
	return dst
}

// ===== 守卫 =====

// TestGeneratedRouteAuthContract 生成路由的鉴权契约守卫。
func TestGeneratedRouteAuthContract(t *testing.T) {
	routes := parseGeneratedRoutes(t)

	// 规则 1：覆盖闭合 —— 每条生成路由要么带强制鉴权，要么登记为公开面。
	//         公开面路由必须**没有**强制鉴权（防"公开端点被误加登录门槛"）。
	for _, r := range routes {
		reason, declaredPublic := publicRoutes[r.key()]
		enforcing := r.enforcing()
		switch {
		case declaredPublic && len(enforcing) > 0:
			t.Errorf("公开端点被误加鉴权：%s 登记为公开面（%s），但有效链上有强制鉴权 %v（mw=%v）",
				r.key(), reason, enforcing, r.mwNames)
		case !declaredPublic && len(enforcing) == 0:
			t.Errorf("生成路由未登记鉴权口径：%s（域 %s，mw=%v）——要么补鉴权中间件，"+
				"要么在 publicRoutes 里登记为公开面并写明理由", r.key(), r.domain, r.mwNames)
		}
	}

	// 规则 2：publicRoutes 不得有陈旧条目（路由已改名/删除 ⇒ 登记失效）。
	existing := map[string]bool{}
	for _, r := range routes {
		existing[r.key()] = true
	}
	for key := range publicRoutes {
		if !existing[key] {
			t.Errorf("publicRoutes 登记陈旧：%s 已不在生成路由里（路由改名/删除后忘记同步）", key)
		}
	}

	// 规则 3：中间件契约 —— 每个生效中间件必须登记，且口径一致；
	//         登记了却没被任何路由用到的（陈旧登记）同样判红。
	used := map[string][]string{} // "<域>.<mw>" → 鉴权口径
	for _, r := range routes {
		for _, name := range r.mwNames {
			key := r.domain + "." + name
			info := mwExprOf(t, r.domain, name)
			if info == "" {
				continue
			}
			used[key] = appendUnique(used[key],
				enforcingAuthRe.FindAllString(info, -1)...)
			used[key] = appendUnique(used[key],
				optionalAuthRe.FindAllString(info, -1)...)
		}
	}
	for key, kinds := range used {
		want, ok := mwContract[key]
		if !ok {
			t.Errorf("生效中间件未登记：%s（口径 %v）——在 mwContract 里登记它提供的鉴权口径", key, kinds)
			continue
		}
		if !sameSet(want, kinds) {
			t.Errorf("中间件鉴权口径变了：%s 期望 %v，实际 %v", key, want, kinds)
		}
	}
	for key, want := range mwContract {
		kinds, ok := used[key]
		if !ok {
			t.Errorf("mwContract 登记陈旧：%s（期望 %v）已不被任何生成路由使用", key, want)
			continue
		}
		if !sameSet(want, kinds) {
			t.Errorf("中间件鉴权口径变了：%s 期望 %v，实际 %v", key, want, kinds)
		}
	}
}

// mwExprOf 取某个域里某个中间件函数的返回表达式源码（不存在返回空串）。
func mwExprOf(t *testing.T, domain, name string) string {
	t.Helper()
	dir := generatedRouterDir(t)
	mws := parseMwFile(t, filepath.Join(dir, domain, "middleware.go"))
	info, ok := mws[name]
	if !ok || info.empty {
		return ""
	}
	return info.expr
}

// sameSet 比较两个字符串集合（顺序无关）。
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string{}, a...)
	y := append([]string{}, b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// TestIdlizedRouteContract 本轮 IDL 化的 10 条路由：钉死所属 IDL 域（= 副本可见性）
// 与有效鉴权口径。
func TestIdlizedRouteContract(t *testing.T) {
	routes := parseGeneratedRoutes(t)
	byKey := map[string]generatedRoute{}
	for _, r := range routes {
		byKey[r.key()] = r
	}
	for key, want := range idlizedRoutes {
		r, ok := byKey[key]
		if !ok {
			t.Errorf("IDL 化路由缺失：%s（生成路由里找不到）", key)
			continue
		}
		if r.domain != want.domain {
			t.Errorf("IDL 化路由的域变了：%s 期望 %s 域，实际 %s 域 —— 域=生成组=副本可见性，"+
				"挪域会让路由在某些副本上凭空出现/消失", key, want.domain, r.domain)
		}
		if !sameSet(want.kinds, r.authKinds) {
			t.Errorf("IDL 化路由的鉴权口径变了：%s 期望 %v，实际 %v", key, want.kinds, r.authKinds)
		}
	}
}

// TestGeneratedRouteAuthEnforcedAtRuntime 行为级对照：静态契约说"受保护"的路由，
// 未带凭据必须被挡；说"公开"的路由，未带凭据不得被鉴权中间件挡（401/403）。
//
// 这是静态守卫的负对照：静态表能看出链上挂了哪个中间件，但看不出它是否真的生效
// （例如中间件写错顺序、或公开端点被同组别的中间件罩住）。
func TestGeneratedRouteAuthEnforcedAtRuntime(t *testing.T) {
	// 与生产一致的最小环境：全局 Recovery（handler 缺装配时 panic → 500 而不是
	// 让测试进程崩），其余中间件不参与本断言。
	prev := config
	config = &conf.AppConfiguration{
		MCP:      conf.MCPConfig{Enabled: true},
		Security: conf.SecurityConfig{OIDC: conf.OIDCConfig{Enabled: true}},
		UI:       conf.UIConfig{ExposeOpenAPI: true},
	}
	defer func() { config = prev }()

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.Use(middleware.Recovery())
	router.GeneratedRegister(h)
	customizedRegister(h)

	// 必须被挡下的（未带凭据 ⇒ 401）
	mustReject := []struct{ method, path string }{
		{"GET", "/api/v1/user/check-auth"},
		{"POST", "/user/api-keys/revoke-all"},
		{"POST", "/api/v1/user/shares/import-local"},
		{"POST", "/api/v1/user/refresh"},
		{"POST", "/api/v1/user/logout"},
	}
	// 不得被鉴权中间件挡下的（未带凭据 ⇒ 不是 401/403）
	mustNotReject := []struct{ method, path string }{
		{"GET", "/api/v1/notifies/public"},
		{"GET", "/api/v1/federation/resolve"},
		{"GET", "/share/metadata/ABCD12"},
		{"POST", "/api/v1/share/multi-direct"},
		{"POST", "/api/v1/share/multi-bind"},
		// 跨站投递（联邦前台化，2026-11-01）：公开面（共享口令在 handler 内校验
		// ⇒ 匿名可达；未带口令时**由 handler 拒绝**，不是被鉴权中间件挡）。
		{"POST", "/api/v1/federation/delivery-slot"},
		{"POST", "/api/v1/federation/deliver"},
	}
	for _, c := range mustReject {
		resp := ut.PerformRequest(h.Engine, c.method, c.path, nil)
		if resp.Code != 401 {
			t.Errorf("%s %s 未带凭据应被挡（401），实际 %d", c.method, c.path, resp.Code)
		}
	}
	for _, c := range mustNotReject {
		resp := ut.PerformRequest(h.Engine, c.method, c.path, nil)
		if resp.Code == 401 || resp.Code == 403 {
			t.Errorf("%s %s 是公开端点，未带凭据却被鉴权挡下（%d）", c.method, c.path, resp.Code)
		}
	}
}

// TestDumpGeneratedRouteAuth 是开发期清单导出（PB_DUMP_ROUTE_AUTH=1 时输出）。
// 常态下跳过：它是造表工具，不是断言。
func TestDumpGeneratedRouteAuth(t *testing.T) {
	if os.Getenv("PB_DUMP_ROUTE_AUTH") != "1" {
		t.Skip("清单导出工具（PB_DUMP_ROUTE_AUTH=1 才运行）")
	}
	for _, r := range parseGeneratedRoutes(t) {
		tag := "AUTH"
		if len(r.enforcing()) == 0 {
			tag = "PUBLIC"
		}
		fmt.Printf("%-10s %-6s %-44s mw=%v auth=%v\n",
			r.domain, tag, r.method+" "+r.path, r.mwNames, r.authKinds)
	}
}
