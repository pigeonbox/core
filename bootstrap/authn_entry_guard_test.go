// 认证入口收口守卫（core#173 第四片：Authentication Policy 收口）。
//
// ## 为什么有这道门禁
//
// pkg/middleware/authn.go 是认证决策的单一执行点（EvaluateJWT：缺失 → 解析 →
// 半会话 → 吊销 → CSRF → 身份回查；ClaimsOfToken/FreshClaimsOfToken/
// HalfSessionClaims 为 handler 窄入口），但**没有任何机制要求新入口调用它**
// ——历史上 UserAuth 自抄一份判定链、session_ops/OIDC 直接摸 pkg/auth 原语，
// 新入口照抄即可绕过收口点，且没有测试会红。
//
// ⇒ 本守卫把「认证决策只准走执行点」变成机械判据（三层，与
// pickup_entry_guard 同款）：
//
//	① 矩阵鉴权列 × 执行点登记：router_policy_matrix.txt（#203 产物，只读）
//	   每条非 public 路由的生效链上必须有**已登记**的强制执行点；链上出现的
//	   全部认证口径必须登记在 authnRouteEntries（承载文件 + 调用标记）——
//	   新中间件绕过执行点/删掉执行点调用 ⇒ 红；登记了却没人用（陈旧）⇒ 红；
//	② 源码扫描 × 裸判定豁免：app/bootstrap/gen/transport 的非测试源码出现
//	   auth.ParseToken( / auth.IsTokenRevoked( / IdentityFresh( ⇒ 红
//	   （handler 不得绕过执行点自判），除非在 authnInlineExemptions 登记理由；
//	   豁免条目对应的文件已无标记（迁移完成忘删）⇒ 陈旧红；
//	③ 手写公开路由 × 登记理由：矩阵里 owner=custom 且 auth=public 的路由
//	   必须全部登记 authnPublicCustomRoutes（生成面公开路由由 router_auth_guard
//	   的 publicRoutes 承接）——手写面新增公开端点不写理由 ⇒ 红。
//
// 负对照：TestAuthnEntryGuardNegativeControl 用合成输入自证三层判定有牙齿
// （恒绿）；真实文件级负对照（删 UserAuth 的 EvaluateJWT 调用 ⇒ ① 红、
// handler 加回 auth.ParseToken ⇒ ② 红，还原 ⇒ 绿）见 PR 正文实跑记录。
//
// 行为级兜底：执行点被掏空（如恒返回 accepted）由 pkg/middleware 既有中间件
// 测试与 router_auth_guard 的 TestGeneratedRouteAuthEnforcedAtRuntime 覆盖。
package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// authnMarker 承载文件的必含标记：Sub 在文件中至少出现 Min 次。
// （Min>1 用于「定义 + 调用」或「多入口都调」的形态，防删掉一处调用仍绿。）
type authnMarker struct {
	Sub string
	Min int
}

// authnCarrier 路由链上的一个认证口径 → 执行点承载文件与必含标记。
type authnCarrier struct {
	Enforcing bool // 是否提供强制鉴权（矩阵非 public 档的候选）
	File      string
	Markers   []authnMarker
}

// authnRouteEntries 路由链上允许出现的全部认证口径 → 执行点承载。
// 名单与 router_auth_guard 的 enforcingAuthRe/optionalAuthRe 同源；
// 新中间件要在路由上提供鉴权，必须在此登记（承载文件真调执行点）。
var authnRouteEntries = map[string]authnCarrier{
	"AdminMiddleware": {Enforcing: true, File: "pkg/middleware/auth.go",
		Markers: []authnMarker{{Sub: "EvaluateJWT(", Min: 3}}}, // Auth/Admin/Optional 三处调用
	"AuthMiddleware": {Enforcing: true, File: "pkg/middleware/auth.go",
		Markers: []authnMarker{{Sub: "EvaluateJWT(", Min: 3}}},
	"AdminOrAdminKey": {Enforcing: true, File: "pkg/middleware/apikey.go",
		Markers: []authnMarker{{Sub: ":= validateAPIKey(", Min: 3}}}, // Key 路径三入口共用
	"UserOrAPIKey": {Enforcing: true, File: "pkg/middleware/apikey.go",
		Markers: []authnMarker{{Sub: ":= validateAPIKey(", Min: 3}}},
	"UserAuth": {Enforcing: true, File: "transport/http/middleware/user_auth.go",
		Markers: []authnMarker{{Sub: "pkgmw.EvaluateJWT(", Min: 1}}},
	"OptionalIdentity": {Enforcing: false, File: "pkg/middleware/apikey.go",
		Markers: []authnMarker{{Sub: "OptionalAuthMiddleware(), OptionalAPIKey()", Min: 1}}},
	"OptionalAuthMiddleware": {Enforcing: false, File: "pkg/middleware/auth.go",
		Markers: []authnMarker{{Sub: "EvaluateJWT(", Min: 3}}},
}

// authnCoreFiles 执行点核心自身的必含标记（防核心文件被掏空/窄入口被删）。
var authnCoreFiles = map[string][]authnMarker{
	"pkg/middleware/authn.go": {
		{Sub: "func EvaluateJWT(", Min: 1},
		{Sub: "func ClaimsOfToken(", Min: 1},
		{Sub: "func FreshClaimsOfToken(", Min: 1},
		{Sub: "func HalfSessionClaims(", Min: 1},
	},
	// scope 按路径判定（AGENTS 硬规则：缺能力 403≠401、缺省全量）：
	// 判定函数定义 + validateAPIKey 内的调用，各至少 1 处 ⇒ Min=2。
	"pkg/middleware/apikey.go": {
		{Sub: "RequiredAPIKeyScope(", Min: 2},
		{Sub: "APIKeyScopeAllowed(", Min: 2},
	},
}

// authnBareMarkers 执行点之外禁止出现的裸认证判定（层②扫描标记）。
var authnBareMarkers = []string{"auth.ParseToken(", "auth.IsTokenRevoked(", "IdentityFresh("}

// authnScanDirs 层②扫描范围（相对仓根；排除 pkg/* ——执行点与原语的家）。
var authnScanDirs = []string{"app", "bootstrap", "gen", "transport"}

// authnInlineExemptions 层②豁免：文件 → 理由。
// 当前为空：迁移后全仓非测试源码已无裸判定；确需 handler 内联的（如换发
// 类端点的特殊序列）必须登记并写明理由，序列收口后删除条目（陈旧即红）。
var authnInlineExemptions = map[string]string{}

// authnPublicCustomRoutes 层③：手写面（routes_custom.go）公开路由 → 登记理由。
var authnPublicCustomRoutes = map[string]string{
	"POST /admin/webauthn/login/options":          "Passkey 登录第一步（公开登录入口，login 桶；core#160）",
	"POST /admin/webauthn/login/verify":           "Passkey 登录第二步（断言在校验器内完成；login 桶）",
	"GET /api/config":                             "前端启动拉取站点配置（公开，无身份面）",
	"GET /api/v1/mcp":                             "MCP 规范：无会话模式 GET 回 405（SDK 应答，不挂鉴权链）",
	"DELETE /api/v1/mcp":                          "MCP 规范：无会话模式 DELETE 回 405（同上）",
	"PUT /api/v1/presign/upload-direct/:uploadID": "预签名直传（授权在签名 URL 内，公开面）",
	"GET /api/v1/user/oidc/callback":              "OIDC 回调（IdP 重定向入口，state HMAC 防 CSRF）",
	"GET /api/v1/user/oidc/login":                 "OIDC 登录入口（访客登录流程）",
	"POST /api/v1/user/oidc/mfa":                  "半会话二验换发（半会话凭据本身即门槛，core#180）",
	"GET /assets/*filepath":                       "前端静态资源（无身份面）",
	"HEAD /assets/*filepath":                      "前端静态资源（无身份面）",
	"GET /openapi.json":                           "OpenAPI 文档（ui.expose_openapi 门控）",
	"GET /preview/:code/thumb":                    "预览缩略图（取件闸门在 app/preview，口令经 X-Share-Password 头）",
	"GET /readyz":                                 "就绪探针",
	"GET /robots.txt":                             "SEO robots（ui.robots_text，公开）",
}

// ===== 推导层 =====

// authnRouteKinds 每条路由的生效鉴权口径（与矩阵推导同源同解析器）。
func authnRouteKinds(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, g := range parseGeneratedRoutes(t) {
		out[g.key()] = g.authKinds
	}
	hw, vars := parseHandwrittenRouteMW(t)
	for k, names := range hw {
		out[k] = policyKinds(names, vars)
	}
	return out
}

// authnScanFiles 层②扫描集：相对路径 → 内容（跳过测试文件）。
func authnScanFiles(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, dir := range authnScanDirs {
		root := filepath.Join("..", dir)
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, rerr := filepath.Rel("..", path)
			if rerr != nil {
				return nil
			}
			if data, rerr := os.ReadFile(path); rerr == nil {
				out[filepath.ToSlash(rel)] = string(data)
			}
			return nil
		})
	}
	if len(out) == 0 {
		t.Fatalf("层②扫描集为空（目录挪窝/走读失败）——守卫自身失效")
	}
	return out
}

// ===== 判定层（纯函数，供主守卫与负对照共用）=====

// authnCheckRoutes 层①：矩阵行 × 路由口径 × 执行点登记 × 承载标记。
func authnCheckRoutes(rows []routePolicy, kinds map[string][]string,
	entries map[string]authnCarrier, read func(string) (string, error)) []string {

	var probs []string
	lookup := func(k string) []string {
		if v, ok := kinds[k]; ok {
			return v
		}
		return kinds[normalizeRouteKey(k)]
	}
	used := map[string]bool{}
	for _, row := range rows {
		ks := lookup(row.key())
		enforcing := false
		for _, k := range ks {
			ent, ok := entries[k]
			if !ok {
				probs = append(probs, fmt.Sprintf(
					"路由 %s 的鉴权口径 %q 未在 authnRouteEntries 登记执行点——认证决策必须经登记的执行点中间件（承载文件+调用标记）",
					row.key(), k))
				continue
			}
			used[k] = true
			if ent.Enforcing {
				enforcing = true
			}
		}
		if row.Auth != "public" && !enforcing {
			probs = append(probs, fmt.Sprintf(
				"矩阵 %s 声明鉴权=%s，但路由链上没有登记的强制执行点（口径=%v）——绕过执行点或矩阵与接线漂移",
				row.key(), row.Auth, ks))
		}
	}
	for k := range entries {
		if !used[k] {
			probs = append(probs, fmt.Sprintf("authnRouteEntries 登记陈旧：%s 已不被任何路由使用", k))
		}
	}
	probs = append(probs, authnCheckMarkers(entries, read)...)
	return probs
}

// authnCheckMarkers 汇总登记口径与核心文件的承载标记，逐文件逐标记断言。
func authnCheckMarkers(entries map[string]authnCarrier, read func(string) (string, error)) []string {
	var probs []string
	agg := map[string][]authnMarker{}
	for _, e := range entries {
		agg[e.File] = append(agg[e.File], e.Markers...)
	}
	for f, ms := range authnCoreFiles {
		agg[f] = append(agg[f], ms...)
	}
	for _, f := range sortedKeys(agg) {
		content, err := read(f)
		if err != nil {
			probs = append(probs, fmt.Sprintf("执行点承载文件 %s 读取失败: %v", f, err))
			continue
		}
		for _, m := range agg[f] {
			if strings.Count(content, m.Sub) < m.Min {
				probs = append(probs, fmt.Sprintf(
					"执行点承载文件 %s 缺少标记 %q（需 ≥%d 处）——判定调用被删或绕过执行点（core#173）",
					f, m.Sub, m.Min))
			}
		}
	}
	return probs
}

// authnScanBareJudgments 层②：裸判定 × 豁免清单（双向对账）。
func authnScanBareJudgments(files map[string]string, exemptions map[string]string) []string {
	var probs []string
	marked := map[string][]string{}
	for _, path := range sortedKeys(files) {
		var hits []string
		for _, m := range authnBareMarkers {
			if strings.Contains(files[path], m) {
				hits = append(hits, m)
			}
		}
		if len(hits) > 0 {
			marked[path] = hits
		}
	}
	for _, path := range sortedKeys(marked) {
		if _, ok := exemptions[path]; !ok {
			probs = append(probs, fmt.Sprintf(
				"源文件 %s 出现裸认证判定 %v——认证决策必须经 pkg/middleware 执行点"+
					"（EvaluateJWT/ClaimsOfToken/FreshClaimsOfToken/HalfSessionClaims），"+
					"或在 authnInlineExemptions 登记理由", path, marked[path]))
		}
	}
	for _, path := range sortedKeys(exemptions) {
		if _, ok := marked[path]; !ok {
			probs = append(probs, fmt.Sprintf(
				"authnInlineExemptions 登记陈旧：%s 已无裸判定标记（收口完成后删除豁免条目）", path))
		}
	}
	return probs
}

// authnCheckPublicCustom 层③：手写公开路由 × 登记理由（双向对账）。
func authnCheckPublicCustom(rows []routePolicy, registry map[string]string) []string {
	var probs []string
	inMatrix := map[string]bool{}
	for _, r := range rows {
		if r.Owner == "custom" && r.Auth == "public" {
			inMatrix[r.key()] = true
		}
	}
	for _, k := range sortedKeys(inMatrix) {
		if _, ok := registry[k]; !ok {
			probs = append(probs, fmt.Sprintf(
				"手写面公开路由 %s 未登记 authnPublicCustomRoutes——新增公开端点必须写明理由"+
					"（中间件层不设门槛、凭据在 handler 内校验的也要写）", k))
		}
	}
	for _, k := range sortedKeys(registry) {
		if !inMatrix[k] {
			probs = append(probs, fmt.Sprintf(
				"authnPublicCustomRoutes 登记陈旧：%s 已不是 custom+public 路由（路由改鉴权/删除后同步）", k))
		}
	}
	return probs
}

// sortedKeys 确定性遍历（错误信息顺序稳定，便于负对照断言）。
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// ===== 守卫 =====

// TestAuthnEntryGuardCoverage 认证入口收口守卫（三层，见文件头）。
func TestAuthnEntryGuardCoverage(t *testing.T) {
	// 矩阵（#203 产物，只读引用）
	raw, err := os.ReadFile(policyMatrixPath(t))
	if err != nil {
		t.Fatalf("读取路由策略矩阵失败: %v", err)
	}
	rows, perr := parsePolicyMatrix(string(raw))
	if perr != nil {
		t.Fatalf("策略矩阵解析失败: %v", perr)
	}
	read := func(rel string) (string, error) {
		b, err := os.ReadFile(filepath.Join("..", rel))
		return string(b), err
	}

	var probs []string
	// ① 矩阵鉴权列 × 执行点登记 × 承载标记
	probs = append(probs, authnCheckRoutes(rows, authnRouteKinds(t), authnRouteEntries, read)...)
	// ② 源码扫描 × 豁免清单
	probs = append(probs, authnScanBareJudgments(authnScanFiles(t), authnInlineExemptions)...)
	// ③ 手写公开路由 × 登记理由
	probs = append(probs, authnCheckPublicCustom(rows, authnPublicCustomRoutes)...)

	for _, p := range probs {
		t.Error(p)
	}
	if len(probs) == 0 {
		t.Logf("认证收口守卫通过：%d 条路由、%d 个执行点登记、%d 个手写公开路由",
			len(rows), len(authnRouteEntries), len(authnPublicCustomRoutes))
	}
}

// TestAuthnEntryGuardNegativeControl 负对照（自证三层判定有牙齿）：
// 合成输入逐维破坏 ⇒ 对应层必报且点名；基准输入 ⇒ 零问题。
// 常态 CI 恒绿——它验证判定逻辑本身；真实文件级负对照见 PR 正文。
func TestAuthnEntryGuardNegativeControl(t *testing.T) {
	// 基准：一条受保护路由（登记口径+承载标记齐全）+ 一条公开路由
	baseRows := []routePolicy{
		{Method: "GET", Path: "/protected", Auth: "admin", Bucket: "global", Owner: "custom"},
		{Method: "GET", Path: "/open", Auth: "public", Bucket: "global", Owner: "custom"},
	}
	baseKinds := map[string][]string{
		"GET /protected": {"RealMw"},
		// 公开路由挂可选身份（OptionalIdentity）是真实形态，也喂进基准
		"GET /open": {"OptionalIdentity"},
	}
	baseEntries := map[string]authnCarrier{
		"RealMw":           {Enforcing: true, File: "carrier.go", Markers: []authnMarker{{Sub: "EvaluateJWT(", Min: 1}}},
		"OptionalIdentity": {Enforcing: false, File: "carrier2.go", Markers: []authnMarker{{Sub: "x", Min: 1}}},
	}
	baseRead := func(rel string) (string, error) {
		switch rel {
		case "carrier.go":
			return "call EvaluateJWT( here", nil
		case "carrier2.go":
			return "x", nil
		}
		// 其余（authnCoreFiles 指向的真实核心文件）从盘上读
		b, err := os.ReadFile(filepath.Join("..", rel))
		return string(b), err
	}
	if probs := authnCheckRoutes(baseRows, baseKinds, baseEntries, baseRead); len(probs) != 0 {
		t.Fatalf("基准输入应零问题，实际: %v", probs)
	}

	// ① 未登记口径（新中间件绕过执行点）
	kinds := map[string][]string{"GET /protected": {"HandRolledAuth"}, "GET /open": {}}
	if probs := authnCheckRoutes(baseRows, kinds, baseEntries, baseRead); !hasProb(probs, "HandRolledAuth") || !hasProb(probs, "未在 authnRouteEntries 登记") {
		t.Errorf("负对照①失败：未登记口径未被点名（probs=%v）", probs)
	}

	// ② 非 public 但链上只有可选身份（绕过强制执行点）
	kinds = map[string][]string{"GET /protected": {"OptionalIdentity"}, "GET /open": {}}
	if probs := authnCheckRoutes(baseRows, kinds, baseEntries, baseRead); !hasProb(probs, "没有登记的强制执行点") {
		t.Errorf("负对照②失败：强制档缺执行点未被点名（probs=%v）", probs)
	}

	// ③ 承载文件删掉执行点调用
	if probs := authnCheckRoutes(baseRows, baseKinds, baseEntries, func(rel string) (string, error) {
		return "no call here", nil
	}); !hasProb(probs, "缺少标记") || !hasProb(probs, "carrier.go") {
		t.Errorf("负对照③失败：承载标记缺失未被点名（probs=%v）", probs)
	}

	// ④ 登记陈旧（口径没人用）
	entries := map[string]authnCarrier{
		"RealMw":  baseEntries["RealMw"],
		"GhostMw": {Enforcing: true, File: "ghost.go", Markers: []authnMarker{{Sub: "x", Min: 1}}},
	}
	kinds = map[string][]string{"GET /protected": {"RealMw"}, "GET /open": {}}
	if probs := authnCheckRoutes(baseRows, kinds, entries, baseRead); !hasProb(probs, "登记陈旧") || !hasProb(probs, "GhostMw") {
		t.Errorf("负对照④失败：陈旧登记未被点名（probs=%v）", probs)
	}

	// ⑤ 裸判定未豁免 / ⑥ 豁免陈旧
	files := map[string]string{"h.go": "auth.ParseToken(tok)"}
	if probs := authnScanBareJudgments(files, map[string]string{}); !hasProb(probs, "裸认证判定") || !hasProb(probs, "h.go") {
		t.Errorf("负对照⑤失败：裸判定未被点名（probs=%v）", probs)
	}
	if probs := authnScanBareJudgments(map[string]string{"h.go": "clean"}, map[string]string{"h.go": "历史理由"}); !hasProb(probs, "登记陈旧") {
		t.Errorf("负对照⑥失败：陈旧豁免未被点名（probs=%v）", probs)
	}

	// ⑦ 手写公开未登记 / ⑧ 登记陈旧
	pubRows := []routePolicy{{Method: "GET", Path: "/newpublic", Auth: "public", Bucket: "global", Owner: "custom"}}
	if probs := authnCheckPublicCustom(pubRows, map[string]string{}); !hasProb(probs, "未登记 authnPublicCustomRoutes") {
		t.Errorf("负对照⑦失败：新增手写公开路由未被点名（probs=%v）", probs)
	}
	if probs := authnCheckPublicCustom([]routePolicy{}, map[string]string{"GET /gone": "理由"}); !hasProb(probs, "登记陈旧") {
		t.Errorf("负对照⑧失败：陈旧公开登记未被点名（probs=%v）", probs)
	}
}

// hasProb 问题清单里是否有包含子串的条目。
func hasProb(items []string, sub string) bool {
	for _, s := range items {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
