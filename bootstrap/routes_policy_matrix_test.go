package bootstrap

// 路由策略矩阵门禁（core#173 本批交付：CI 路由鉴权 × 频控矩阵）。
//
// ## 要解决的结构性缺口
//
// issue 正文：「任何新增公开端点必须声明所需策略（鉴权级别 × 频控桶 × 归属），
// 缺声明 ⇒ 提交即红」。仓内已有的守卫各管一维，但没有一张**全路由三维快照**：
//
//   - routes_guard_test.go          只钉「路由存在性」（方法×路径契约表）；
//   - router_auth_guard_test.go     只解析**生成面**（gen/router）的鉴权接线；
//   - ratelimit_bucket_test.go      只守「桶闭合」（前缀 ↔ 路由双向）；
//   - routes_classification_test.go 只管**手写面**的 IDL 豁免理由。
//
// 手写面（routes_custom.go）的鉴权此前只散落在注释里；「这条路由归哪个域、
// 吃哪个限流桶、要什么鉴权」没有单一可 diff 的真相源 ⇒ 策略漂移只能靠人肉 review。
//
// ## 本守卫的判据
//
//	① 扫描真实注册集（GeneratedRegister + customizedRegister，与既有守卫同配置），
//	   对每条路由机械推导三维：
//	     鉴权   —— 生成面走 parseGeneratedRoutes 的 AST 链（与 router_auth_guard
//	              同源同口径）；手写面走 customizedRegister 的 AST 注册调用
//	              （中间件实参 + 局部变量展开，如 authMW ⇒ AdminMiddleware）；
//	      频控桶 —— rateLimitBucket()（与 ratelimit 守卫同源），
//	              空桶再分 exempt（rateLimitGlobalExempt）/ global（全局默认桶）；
//	      归属   —— 生成面 = IDL 域（gen/router/<domain>）；手写面 = custom。
//	② 快照文件 router_policy_matrix.txt 与推导结果**逐行逐列相等**：
//	     · 路由存在、矩阵缺行 ⇒ 红（点名路由 + 补声明位置）；
//	     · 矩阵有行、路由已删 ⇒ 红（陈旧声明）；
//	     · 任一列与代码不一致 ⇒ 红（点名列名与两侧取值）；
//	     · 顺序/格式漂移 ⇒ 红（字节级比对兜底）。
//	③ 负对照（TestRouterPolicyMatrixNegativeControl）：从矩阵删一条 ⇒ 必报
//	   「缺声明」；改一列 ⇒ 必报列不一致 —— 守卫自证有牙齿。
//
// ## 与既有分类口径的对齐
//
// 鉴权取值与 router_auth_guard 的 enforcing/public 划分逐字同源（同一组正则、
// 同一批 parse 结果）；频控桶与 ratelimit 守卫共用 rateLimitBucket/rateLimitGlobalExempt；
// 手写豁免与 routes_classification 的 handwrittenRouteExemptions 互不重叠
// （那边管「为什么不在 IDL」，这边管「生效策略是什么」）。
//
// ## 重新生成 / 补声明
//
//	PB_UPDATE_ROUTE_POLICY_MATRIX=1 go test ./bootstrap -run TestRouterPolicyMatrix
//
// 或手工在 router_policy_matrix.txt 按同格式加行（必须与代码实际一致，否则报红）。
// 路由有意变更时矩阵 diff 即策略 diff —— PR 评审直接看它。

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/gen/router"
)

// policyMatrixFile 矩阵快照文件名（与本测试同目录）。
const policyMatrixFile = "router_policy_matrix.txt"

// policyUpdateEnv 置 1 时重新生成快照文件。
const policyUpdateEnv = "PB_UPDATE_ROUTE_POLICY_MATRIX"

// routePolicy 一条路由的三维策略（矩阵的一行）。
type routePolicy struct {
	Method string
	Path   string
	// Auth 中间件层鉴权级别：admin | jwt | jwt-or-apikey | public。
	// 口令/令牌在 handler 内校验的端点（中间件层不设门槛）登记为 public。
	Auth string
	// Bucket 限流桶：login | upload | download | global | exempt。
	Bucket string
	// Owner 归属：IDL 域名（gen/router 生成面）| custom（routes_custom.go 手写区）。
	Owner string
}

func (p routePolicy) key() string { return p.Method + " " + p.Path }

// ===== 推导层 =====

// buildRoutePolicies 扫描真实注册集，机械推导每条路由的策略三维。
func buildRoutePolicies(t *testing.T) []routePolicy {
	t.Helper()

	// 与 routes_guard / ratelimit 守卫同款最小配置：全部注册门控打开，
	// 拿到「单机模式下最全」的路由集。
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

	var keys []string
	seen := map[string]bool{}
	for _, r := range h.Routes() {
		k := r.Method + " " + r.Path
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		t.Fatal("路由表为空，测试环境装配失败（守卫自身失效）")
	}

	// 生成面索引：key → 生成路由画像（域 + 鉴权口径，与 router_auth_guard 同一解析器）。
	// 归一化键兜底 hertz 与 IDL 的尾斜杠差异（实录：/share/file/ vs /share/file）。
	genIdx := map[string]generatedRoute{} // 归一化 key → 画像
	genExact := map[string]generatedRoute{}
	for _, g := range parseGeneratedRoutes(t) {
		genExact[g.key()] = g
		genIdx[normalizeRouteKey(g.key())] = g
	}

	// 手写面索引：key → 中间件名（含局部变量展开，如 authMW ⇒ AdminMiddleware）。
	hwExact, hwVars := parseHandwrittenRouteMW(t)
	hwIdx := map[string][]string{}
	for k, names := range hwExact {
		hwIdx[normalizeRouteKey(k)] = names
	}

	lookupGen := func(k string) (generatedRoute, bool) {
		if v, ok := genExact[k]; ok {
			return v, true
		}
		v, ok := genIdx[normalizeRouteKey(k)]
		return v, ok
	}
	lookupHW := func(k string) ([]string, bool) {
		if v, ok := hwExact[k]; ok {
			return v, true
		}
		v, ok := hwIdx[normalizeRouteKey(k)]
		return v, ok
	}

	out := make([]routePolicy, 0, len(keys))
	for _, k := range keys {
		method, p, _ := strings.Cut(k, " ")
		var auth, owner string
		if g, ok := lookupGen(k); ok {
			auth, owner = policyAuthLevel(g.authKinds), g.domain
		} else if names, ok := lookupHW(k); ok {
			auth, owner = policyAuthLevel(policyKinds(names, hwVars)), "custom"
		} else if strings.HasPrefix(p, "/assets/") {
			// StaticFS 注册（框架按文件通配，非 r.METHOD 字面量调用）。
			auth, owner = "public", "custom"
		} else {
			t.Fatalf("路由 %q 无法归属：既不在 gen/router 解析结果，也不在 routes_custom.go 手写解析里。"+
				"若这是新注册方式（StaticFS / 新辅助函数），请在 bootstrap/routes_policy_matrix_test.go "+
				"的归属分支补解析，再重新生成矩阵", k)
		}
		out = append(out, routePolicy{
			Method: method, Path: p,
			Auth: auth, Bucket: policyBucket(p), Owner: owner,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// normalizeRouteKey 去尾斜杠（根路径除外），用于 hertz 注册形态 ↔ IDL 字面量的对齐。
func normalizeRouteKey(k string) string {
	m, p, ok := strings.Cut(k, " ")
	if !ok {
		return k
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}
	return m + " " + p
}

// policyAuthLevel 鉴权口径集合 → 矩阵鉴权级别（强制优先，与 enforcingAuthRe 同源）。
func policyAuthLevel(kinds []string) string {
	has := func(names ...string) bool {
		for _, n := range names {
			for _, k := range kinds {
				if k == n {
					return true
				}
			}
		}
		return false
	}
	switch {
	case has("AdminMiddleware", "AdminOrAdminKey", "RequireAdmin"):
		return "admin"
	case has("UserOrAPIKey"):
		return "jwt-or-apikey"
	case has("AuthMiddleware", "UserAuth", "RequireRole", "RequireAnyRole"):
		return "jwt"
	default:
		// 无强制鉴权：OptionalIdentity/OptionalAuth 等可选身份，或完全裸奔 ⇒ 公开面。
		return "public"
	}
}

// policyBucket 路径 → 矩阵频控桶。空桶按 rateLimitGlobalExempt 二分为
// exempt（探针/静态豁免）与 global（全局默认桶兜底）。
func policyBucket(path string) string {
	if b := rateLimitBucket(path); b != "" {
		return b
	}
	if rateLimitGlobalExempt(path) {
		return "exempt"
	}
	return "global"
}

// policyKinds 手写面中间件名 → 鉴权口径（复用生成面的两组正则；
// 局部变量（authMW）展开为其全部赋值的中间件名）。
func policyKinds(names []string, vars map[string][]string) []string {
	var all []string
	expand := func(n string) {
		all = appendUnique(all, n)
		for _, v := range vars[n] {
			all = appendUnique(all, v)
		}
	}
	for _, n := range names {
		expand(n)
	}
	joined := strings.Join(all, " ")
	kinds := enforcingAuthRe.FindAllString(joined, -1)
	return appendUnique(kinds, optionalAuthRe.FindAllString(joined, -1)...)
}

// parseHandwrittenRouteMW 解析 routes_custom.go 的 customizedRegister：
// 返回「METHOD path → 中间件名列表」与「局部变量 → 赋值过的中间件名」。
func parseHandwrittenRouteMW(t *testing.T) (map[string][]string, map[string][]string) {
	t.Helper()
	path := filepath.Join(generatedRouterDir(t), "..", "..", "bootstrap", "routes_custom.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("解析手写路由文件失败 %s: %v", path, err)
	}
	var fn *ast.FuncDecl
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if ok && fd.Name.Name == "customizedRegister" && fd.Body != nil {
			fn = fd
			break
		}
	}
	if fn == nil {
		t.Fatalf("%s 里找不到 customizedRegister —— 函数改名/挪窝后本守卫的归属解析失效", path)
	}

	// 局部变量赋值：authMW := middleware.AdminMiddleware() / authMW = middleware.OptionalAuthMiddleware()
	vars := map[string][]string{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		s, ok := n.(*ast.AssignStmt)
		if !ok || len(s.Lhs) != 1 || len(s.Rhs) != 1 {
			return true
		}
		id, ok := s.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}
		if call, ok := s.Rhs[0].(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				vars[id.Name] = appendUnique(vars[id.Name], sel.Sel.Name)
			}
		}
		return true
	})

	routes := map[string][]string{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		s, ok := n.(*ast.ExprStmt)
		if !ok {
			return true
		}
		call, ok := s.X.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !httpMethods[sel.Sel.Name] {
			return true
		}
		// 只认注册器 r 上的调用（排除 metricsServer.GET 这类旁路 server）。
		x, ok := sel.X.(*ast.Ident)
		if !ok || x.Name != "r" {
			return true
		}
		p := stringArg(fset, call.Args, 0)
		if p == "" {
			return true // 非字面量路径（动态 metricsPath 等）不在本矩阵口径内
		}
		var names []string
		for _, a := range call.Args[1:] {
			switch v := a.(type) {
			case *ast.Ident:
				names = append(names, v.Name) // 裸变量：authMW
			case *ast.CallExpr:
				switch fun := v.Fun.(type) {
				case *ast.SelectorExpr:
					names = append(names, fun.Sel.Name) // middleware.AdminOrAdminKey()
				case *ast.Ident:
					names = append(names, fun.Name) // mcpGuestGate()
				}
			}
		}
		routes[sel.Sel.Name+" "+p] = appendUnique(routes[sel.Sel.Name+" "+p], names...)
		return true
	})
	return routes, vars
}

// ===== 快照层 =====

// policyMatrixPath 快照文件绝对路径（与本测试同目录）。
func policyMatrixPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(generatedRouterDir(t), "..", "..", "bootstrap", policyMatrixFile)
}

// renderPolicyMatrix 矩阵 → 快照文件内容（确定性排序，可 diff）。
func renderPolicyMatrix(entries []routePolicy) string {
	var b strings.Builder
	b.WriteString("# PigeonBox core 路由策略矩阵（core#173：新增端点必须声明 鉴权级别 × 频控桶 × 归属）\n")
	b.WriteString("# 本文件由 TestRouterPolicyMatrix 生成/校验；路由变了而本文件没同步 ⇒ CI 红。\n")
	b.WriteString("# 列：METHOD PATH | 鉴权 | 频控桶 | 归属\n")
	b.WriteString("#   鉴权   admin | jwt | jwt-or-apikey | public（中间件层口径；口令/令牌在 handler 内校验的端点登记为 public）\n")
	b.WriteString("#   频控桶 login | upload | download | global（全局默认桶兜底）| exempt（探针/静态豁免）——来源 bootstrap/middleware.go\n")
	b.WriteString("#   归属   <IDL 域>（gen/router 生成面）| custom（bootstrap/routes_custom.go 手写区）\n")
	b.WriteString("# 重新生成：PB_UPDATE_ROUTE_POLICY_MATRIX=1 go test ./bootstrap -run TestRouterPolicyMatrix\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "%s | %s | %s | %s\n", e.key(), e.Auth, e.Bucket, e.Owner)
	}
	return b.String()
}

// parsePolicyMatrix 快照内容 → 条目（跳过 # 注释与空行；格式错报行号）。
func parsePolicyMatrix(content string) ([]routePolicy, error) {
	var out []routePolicy
	for i, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		parts := strings.Split(trimmed, " | ")
		if len(parts) != 4 {
			return nil, fmt.Errorf("第 %d 行格式错误（应为 `METHOD PATH | 鉴权 | 频控桶 | 归属`）: %q", i+1, line)
		}
		method, p, ok := strings.Cut(parts[0], " ")
		if !ok || method == "" || p == "" {
			return nil, fmt.Errorf("第 %d 行缺方法或路径: %q", i+1, line)
		}
		out = append(out, routePolicy{
			Method: method, Path: p,
			Auth: parts[1], Bucket: parts[2], Owner: parts[3],
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("矩阵为空（没有任何策略条目）")
	}
	return out, nil
}

// comparePolicyMatrix 双向比对推导结果与声明，返回问题清单（空 = 一致）。
// 供主守卫与负对照共用 —— 负对照删/改条目后必须产出非空清单。
func comparePolicyMatrix(derived, declared []routePolicy) []string {
	dIdx := map[string]routePolicy{}
	for _, p := range derived {
		dIdx[p.key()] = p
	}
	sIdx := map[string]routePolicy{}
	for _, p := range declared {
		sIdx[p.key()] = p
	}

	var probs []string
	for k, d := range dIdx {
		s, ok := sIdx[k]
		if !ok {
			probs = append(probs, fmt.Sprintf(
				"缺声明：%s —— 新增路由必须声明策略。在 %s 补一行（格式 `METHOD PATH | 鉴权 | 频控桶 | 归属`）"+
					"或运行 %s=1 go test ./bootstrap -run TestRouterPolicyMatrix 重新生成",
				k, policyMatrixFile, policyUpdateEnv))
			continue
		}
		for _, col := range []struct{ name, want, got string }{
			{"鉴权", d.Auth, s.Auth},
			{"频控桶", d.Bucket, s.Bucket},
			{"归属", d.Owner, s.Owner},
		} {
			if col.want != col.got {
				probs = append(probs, fmt.Sprintf(
					"策略漂移：%s 的%s与代码不一致（矩阵=%q 实际=%q）——"+
						"路由策略有意变更时重新生成矩阵并在 PR 里说明，无意漂移则改回代码侧",
					k, col.name, col.got, col.want))
			}
		}
	}
	for k := range sIdx {
		if _, ok := dIdx[k]; !ok {
			probs = append(probs, fmt.Sprintf(
				"陈旧声明：%s 对应的路由已不存在 —— 删除 %s 里的该行或重新生成", k, policyMatrixFile))
		}
	}
	sort.Strings(probs)
	return probs
}

// ===== 守卫 =====

// TestRouterPolicyMatrix 路由策略矩阵门禁（见文件头注释）。
func TestRouterPolicyMatrix(t *testing.T) {
	derived := buildRoutePolicies(t)
	path := policyMatrixPath(t)

	if os.Getenv(policyUpdateEnv) == "1" {
		if err := os.WriteFile(path, []byte(renderPolicyMatrix(derived)), 0o644); err != nil {
			t.Fatalf("写入矩阵快照失败 %s: %v", path, err)
		}
		t.Logf("已重新生成 %s（%d 条）", path, len(derived))
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取策略矩阵快照失败 %s: %v —— 首次生成：%s=1 go test ./bootstrap -run TestRouterPolicyMatrix",
			path, err, policyUpdateEnv)
	}
	declared, perr := parsePolicyMatrix(string(raw))
	if perr != nil {
		t.Fatalf("策略矩阵快照解析失败 %s: %v", path, perr)
	}

	probs := comparePolicyMatrix(derived, declared)
	for _, p := range probs {
		t.Error(p)
	}
	// 字节级兜底：字段全对但顺序/格式漂移同样判红（保证快照始终可 diff）。
	if len(probs) == 0 && string(raw) != renderPolicyMatrix(derived) {
		t.Errorf("%s 与生成内容不一致（顺序或格式漂移）—— 运行 %s=1 重新生成",
			path, policyUpdateEnv)
	}
	t.Logf("矩阵 %d 条：auth 分布与桶分布见 -v（快照 %s）", len(derived), filepath.Base(path))
}

// TestRouterPolicyMatrixNegativeControl 负对照（自证有牙齿）：
//  1. 从矩阵删一条 ⇒ 必报「缺声明」且点名该路由；
//  2. 改一列（鉴权） ⇒ 必报「策略漂移」且点名列名。
//
// 常态 CI 里它恒绿 —— 它验证的是 comparePolicyMatrix 的判定逻辑本身；
// 真实文件的删行红由 TestRouterPolicyMatrix 覆盖（人工负对照见 PR 正文）。
func TestRouterPolicyMatrixNegativeControl(t *testing.T) {
	derived := buildRoutePolicies(t)
	if len(derived) < 2 {
		t.Fatalf("矩阵条目仅 %d 条，负对照无意义（守卫自身失效）", len(derived))
	}

	// ① 删一条
	victim := derived[len(derived)/2]
	without := make([]routePolicy, 0, len(derived)-1)
	for _, p := range derived {
		if p.key() != victim.key() {
			without = append(without, p)
		}
	}
	probs := comparePolicyMatrix(derived, without)
	if !containsSub(probs, "缺声明") || !containsSub(probs, victim.key()) {
		t.Errorf("负对照①失败：从矩阵删掉 %s 后守卫未点名报缺声明（probs=%v）—— 判定逻辑无牙齿", victim.key(), probs)
	}

	// ② 改一列
	mutated := append([]routePolicy{}, derived...)
	for i := range mutated {
		if mutated[i].key() == victim.key() {
			if mutated[i].Auth == "public" {
				mutated[i].Auth = "admin"
			} else {
				mutated[i].Auth = "public"
			}
			break
		}
	}
	probs = comparePolicyMatrix(derived, mutated)
	if !containsSub(probs, "策略漂移") || !containsSub(probs, victim.key()) || !containsSub(probs, "鉴权") {
		t.Errorf("负对照②失败：篡改 %s 的鉴权列后守卫未点名报漂移（probs=%v）—— 判定逻辑无牙齿", victim.key(), probs)
	}
}

// containsSub 清单里是否有包含子串的条目。
func containsSub(items []string, sub string) bool {
	for _, s := range items {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
