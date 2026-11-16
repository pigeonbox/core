// 授权入口收口守卫（core#173 第五片：Authorization Policy 收口）。
//
// ## 为什么有这道门禁
//
// pkg/authz 是授权判定的单一执行点（IsAdminRole/IsActiveAdmin/RoleSatisfies/
// AnyRoleSatisfies 角色核 + OwnedByUser/OwnedByIP 归属核），但**没有任何机制
// 要求新入口调用它**——历史上「admin」字面量散在 8 个文件各写一份、归属拒绝
// 惯用式（UserID == nil || *UserID != caller）在 handler 里各抄一份，新入口照抄
// 即可绕过收口点，且没有任何测试会红。
//
// ⇒ 本守卫把「角色/归属判定只准走 pkg/authz」变成机械判据（三层，与
// authn_entry_guard 同款）：
//
//	① 路由链角色口径 × 执行点登记 × 承载标记：router_policy_matrix.txt（#203
//	   产物，只读）里每条路由链上的角色关口径（AdminMiddleware/AdminOrAdminKey/
//	   RequireAdmin/RequireRole/RequireAnyRole）必须登记在 authzRouteEntries
//	   （承载文件 + authz 调用标记）；矩阵 auth=admin 的路由链上必须有已登记的
//	   **强制**角色关；非休眠登记没人用 ⇒ 陈旧红。新角色中间件绕过执行点/承载
//	   文件删掉 authz 调用 ⇒ 红；RequireAdmin 等当前无路由使用但存在 ⇒ Dormant
//	   登记（参与标记检查，防未来裸用；跳过陈旧检查）；
//	② 源码扫描 × 裸判定豁免：app/bootstrap/gen/transport/pkg（排除 pkg/authz
//	   执行点自身）的非测试源码出现裸角色判定（Role != "admin"、GetUserRole(c)
//	   != ...）或裸归属判定（UserID == nil || *UserID !=、*x.UserID 解引用比较、
//	   OwnerIP 比较）⇒ 红，除非在 authzInlineExemptions 登记理由；豁免条目已无
//	   标记 ⇒ 陈旧红；
//	③ 判定站点登记 × 承载标记 + 执行点核心自检 + 反向对账：所有必须经执行点的
//	   判定站点（登录/Passkey/MCP 能力位/分享删除/时间线/object_key/分片三通道…）
//	   登记 authzSites——承载文件删掉 authz 调用 ⇒ 红；源码出现 authz. 调用但
//	   文件未登记 ⇒ 红（新站点必须进清单，PR 散点清单随之更新）；
//	   pkg/authz/authz.go 六个判定函数定义缺失 ⇒ 执行点被掏空红。
//
// 负对照：TestAuthzEntryGuardNegativeControl 用合成输入自证三层判定有牙齿
// （恒绿）；真实文件级负对照（删 AdminMiddleware 的 authz.IsAdminRole 调用 ⇒
// ①③ 红、还原 ⇒ 绿；handler 加回裸 Role != "admin" ⇒ ② 红）见 PR 正文实跑记录。
//
// 行为级兜底：执行点被掏空（如 IsAdminRole 恒返回 true）由 pkg/authz 单测与
// 既有中间件测试（admin 403 断言）覆盖。
package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// authzMarker 承载文件的必含标记：Sub 在文件中至少出现 Min 次。
type authzMarker struct {
	Sub string
	Min int
}

// authzCarrier 路由链上的一个角色关口径 → 执行点承载文件与必含标记。
type authzCarrier struct {
	// Dormant 当前无路由使用但**存在**（RequireAdmin/RequireRole/RequireAnyRole）：
	// 参与承载标记检查（防未来裸用），跳过「登记陈旧」检查。
	Dormant bool
	File    string
	Markers []authzMarker
}

// authzRouteEntries 路由链上允许出现的全部角色关口径 → 执行点承载。
// 名单与 router_auth_guard 的 enforcingAuthRe 里角色相关子集同源；
// 新中间件要在路由上提供角色关，必须在此登记（承载文件真调 pkg/authz）。
var authzRouteEntries = map[string]authzCarrier{
	"AdminMiddleware": {File: "pkg/middleware/auth.go",
		Markers: []authzMarker{{Sub: "authz.IsAdminRole(", Min: 1}}},
	"AdminOrAdminKey": {File: "pkg/middleware/apikey.go",
		Markers: []authzMarker{{Sub: "authz.IsAdminRole(", Min: 1}}},
	// 判定核在 IsAdmin（transport/http/middleware/auth.go），RequireAdmin 只是调用方。
	"RequireAdmin": {Dormant: true, File: "transport/http/middleware/auth.go",
		Markers: []authzMarker{{Sub: "authz.IsAdminRole(", Min: 1}}},
	"RequireRole": {Dormant: true, File: "transport/http/middleware/user_auth.go",
		Markers: []authzMarker{{Sub: "authz.RoleSatisfies(", Min: 1}}},
	"RequireAnyRole": {Dormant: true, File: "transport/http/middleware/user_auth.go",
		Markers: []authzMarker{{Sub: "authz.AnyRoleSatisfies(", Min: 1}}},
}

// authzRoleKinds 角色关口径集合（路由链上出现这些名字即必须登记）。
var authzRoleKinds = map[string]bool{
	"AdminMiddleware": true, "AdminOrAdminKey": true, "RequireAdmin": true,
	"RequireRole": true, "RequireAnyRole": true,
}

// authzCoreFiles 执行点核心自身的必含标记（防 pkg/authz 被掏空/函数被删）。
var authzCoreFiles = map[string][]authzMarker{
	"pkg/authz/authz.go": {
		{Sub: "func IsAdminRole(", Min: 1},
		{Sub: "func IsActiveAdmin(", Min: 1},
		{Sub: "func RoleSatisfies(", Min: 1},
		{Sub: "func AnyRoleSatisfies(", Min: 1},
		{Sub: "func OwnedByUser(", Min: 1},
		{Sub: "func OwnedByIP(", Min: 1},
	},
}

// authzSite 一个必须经执行点的判定站点（承载文件 + 必含 authz 调用标记）。
type authzSite struct {
	File    string
	Markers []authzMarker
}

// authzSites 判定站点清单（= PR 散点迁移清单的机械对账面）。
// 新增角色/归属判定站点必须登记；站点文件删掉 authz 调用 ⇒ 红。
var authzSites = map[string]authzSite{
	"admin 登录角色门": {File: "app/admin/auth.go",
		Markers: []authzMarker{{Sub: "authz.IsAdminRole(", Min: 1}}},
	"Passkey 在职管理员（四入口）": {File: "app/admin/webauthn.go",
		Markers: []authzMarker{{Sub: "authz.IsActiveAdmin(", Min: 4}}},
	"OIDC 半会话 admin 条件": {File: "app/oidc/service.go",
		Markers: []authzMarker{{Sub: "authz.IsAdminRole(", Min: 1}}},
	"MCP Caller 能力位": {File: "bootstrap/routes_custom.go",
		Markers: []authzMarker{{Sub: "authz.IsAdminRole(", Min: 1}}},
	"分享删除属主": {File: "app/share/service.go",
		Markers: []authzMarker{{Sub: "authz.OwnedByUser(", Min: 1}}},
	"取件时间线属主": {File: "app/share/pickup_timeline.go",
		Markers: []authzMarker{{Sub: "authz.OwnedByUser(", Min: 1}}},
	"presign object_key 属主": {File: "transport/http/handler/share_multi.go",
		Markers: []authzMarker{{Sub: "authz.OwnedByUser(", Min: 1}, {Sub: "authz.OwnedByIP(", Min: 1}}},
	"分片会话三通道归属": {File: "app/chunk/token.go",
		Markers: []authzMarker{{Sub: "authz.OwnedByUser(", Min: 3}, {Sub: "authz.OwnedByIP(", Min: 2}}},
}

// authzBareRes 裸判定扫描标记（层②）。
// 只拦「判定形态」：与 admin 字面量/上下文角色的比较、归属拒绝惯用式、
// 用户 ID 解引用比较、OwnerIP 比较。入参值域校验/SQL 过滤/统计去重不在此列
// （见 authzInlineExemptions 豁免理由）。
var authzBareRes = []struct {
	Name string
	Re   *regexp.Regexp
}{
	{"角色-admin字面量比较", regexp.MustCompile(`[Rr]ole\s*[!=]=\s*"admin"`)},
	{"上下文角色比较", regexp.MustCompile(`GetUserRole\([^)]*\)\s*[!=]=`)},
	{"ctx角色比较", regexp.MustCompile(`RoleFromContext\([^)]*\)\s*[!=]=`)},
	{"归属拒绝惯用式", regexp.MustCompile(`UserID\s*==\s*nil\s*\|\|\s*\*[A-Za-z_][A-Za-z0-9_]*\.UserID\s*!=`)},
	{"UserID解引用比较(左)", regexp.MustCompile(`\*[A-Za-z_][A-Za-z0-9_]*\.UserID\s*[!=]=`)},
	{"UserID解引用比较(右)", regexp.MustCompile(`[!=]=\s*\*[A-Za-z_][A-Za-z0-9_]*\.UserID`)},
	{"解引用用户比对", regexp.MustCompile(`\*[A-Za-z_][A-Za-z0-9_]*\s*[!=]=\s*\*?[A-Za-z_][A-Za-z0-9_]*\.UserID`)},
	{"OwnerIP比较", regexp.MustCompile(`OwnerIP\s*[!=]=`)},
}

// authzScanDirs 层②扫描范围（相对仓根；pkg/authz 执行点自身除外）。
var authzScanDirs = []string{"app", "bootstrap", "gen", "transport", "pkg", "repo"}

// authzInlineExemptions 层②豁免：文件 → 理由。
// 豁免只给「不是授权判定」的比较（入参值域/统计口径/状态回写/执行点内部
// 空属主分支），不给裸判定开后门。
var authzInlineExemptions = map[string]string{
	"gen/handler/admin/admin_manage.go":     "请求体 role 字段值域校验（admin/user 枚举），非调用方角色判定；路由已在 AdminMiddleware 后",
	"app/admin/files.go":                    "统计聚合去重（*f.UserID == 0 判无主行），非归属判定",
	"app/chunk/service.go":                  "会话接管**已验证归属之后**的 owner_ip 回写（状态管理），非授权门",
	"repo/db/dao/filecode_admin.go":         `管理面筛选参数构造（q.OwnerIP != ""），查询过滤非判定`,
	"app/chunk/token.go":                    `执行点自身的空属主分支（OwnerIP == "" 老数据 fail-closed），匹配判定已走 authz.OwnedByIP/OwnedByUser`,
	"transport/http/handler/share_multi.go": `历史会话无属主放行分支（meta.OwnerIP == ""），匹配判定已走 authz.OwnedByUser/OwnedByIP`,
}

// ===== 推导层 =====

// authzRouteKinds 每条路由的角色关口径（复用 authn 守卫的同源解析器）。
func authzRouteKinds(t *testing.T) map[string][]string {
	return authnRouteKinds(t)
}

// authzScanFiles 层②扫描集：相对路径 → 内容（跳过测试文件与 pkg/authz 自身）。
func authzScanFiles(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, dir := range authzScanDirs {
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
			slash := filepath.ToSlash(rel)
			if strings.HasPrefix(slash, "pkg/authz/") {
				return nil // 执行点自身（role == "admin" 是定义不是判定）
			}
			if data, rerr := os.ReadFile(path); rerr == nil {
				out[slash] = string(data)
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

// authzCheckRoutes 层①：矩阵行 × 路由口径 × 执行点登记 × 承载标记。
func authzCheckRoutes(kinds map[string][]string,
	entries map[string]authzCarrier, rows []routePolicy,
	read func(string) (string, error)) []string {

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
			if !authzRoleKinds[k] {
				continue
			}
			ent, ok := entries[k]
			if !ok {
				probs = append(probs, fmt.Sprintf(
					"路由 %s 的角色关口径 %q 未在 authzRouteEntries 登记执行点——角色判定必须经登记的执行点（承载文件+authz 调用标记）",
					row.key(), k))
				continue
			}
			used[k] = true
			if !ent.Dormant {
				enforcing = true
			}
		}
		// 矩阵 auth=admin ⇒ 链上必须有已登记的强制角色关（矩阵与接线漂移检测）。
		if row.Auth == "admin" && !enforcing {
			probs = append(probs, fmt.Sprintf(
				"矩阵 %s 声明鉴权=admin，但路由链上没有登记的强制角色关口径（口径=%v）——绕过角色关或矩阵与接线漂移",
				row.key(), ks))
		}
	}
	for k, ent := range entries {
		if !used[k] && !ent.Dormant {
			probs = append(probs, fmt.Sprintf("authzRouteEntries 登记陈旧：%s 已不被任何路由使用", k))
		}
	}
	// 承载标记（仅登记口径的文件；站点与核心由 authzCheckSites 负责）。
	agg := map[string][]authzMarker{}
	for _, e := range entries {
		agg[e.File] = append(agg[e.File], e.Markers...)
	}
	probs = append(probs, authzCheckMarkers(agg, read)...)
	return probs
}

// authzCheckMarkers 逐文件逐标记断言（缺标记 = 判定调用被删/绕过执行点）。
func authzCheckMarkers(agg map[string][]authzMarker, read func(string) (string, error)) []string {
	var probs []string
	for _, f := range sortedKeys(agg) {
		content, err := read(f)
		if err != nil {
			probs = append(probs, fmt.Sprintf("执行点承载文件 %s 读取失败: %v", f, err))
			continue
		}
		for _, m := range agg[f] {
			if strings.Count(content, m.Sub) < m.Min {
				probs = append(probs, fmt.Sprintf(
					"执行点承载文件 %s 缺少标记 %q（需 ≥%d 处）——判定调用被删或绕过 pkg/authz 执行点（core#173 第五片）",
					f, m.Sub, m.Min))
			}
		}
	}
	return probs
}

// authzScanBareJudgments 层②：裸判定 × 豁免清单（双向对账）。
func authzScanBareJudgments(files map[string]string, exemptions map[string]string) []string {
	var probs []string
	marked := map[string][]string{}
	for _, path := range sortedKeys(files) {
		var hits []string
		for _, br := range authzBareRes {
			if br.Re.MatchString(files[path]) {
				hits = append(hits, br.Name)
			}
		}
		if len(hits) > 0 {
			marked[path] = hits
		}
	}
	for _, path := range sortedKeys(marked) {
		if _, ok := exemptions[path]; !ok {
			probs = append(probs, fmt.Sprintf(
				"源文件 %s 出现裸授权判定 %v——角色/归属判定必须经 pkg/authz 执行点"+
					"（IsAdminRole/IsActiveAdmin/RoleSatisfies/AnyRoleSatisfies/OwnedByUser/OwnedByIP），"+
					"或在 authzInlineExemptions 登记理由", path, marked[path]))
		}
	}
	for _, path := range sortedKeys(exemptions) {
		if _, ok := marked[path]; !ok {
			probs = append(probs, fmt.Sprintf(
				"authzInlineExemptions 登记陈旧：%s 已无裸判定标记（迁移完成后删除豁免条目）", path))
		}
	}
	return probs
}

// authzCheckSites 层③：站点/核心标记 + 反向对账（authz 调用文件必须登记）。
func authzCheckSites(files map[string]string, read func(string) (string, error)) []string {
	var probs []string
	// 正向：登记站点与执行点核心的承载文件必须含 authz 调用标记。
	agg := map[string][]authzMarker{}
	for _, name := range sortedKeys(authzSites) {
		s := authzSites[name]
		agg[s.File] = append(agg[s.File], s.Markers...)
	}
	for f, ms := range authzCoreFiles {
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
					"执行点承载文件 %s 缺少标记 %q（需 ≥%d 处）——判定调用被删或绕过 pkg/authz 执行点（core#173 第五片）",
					f, m.Sub, m.Min))
			}
		}
	}
	// 反向：源码出现 authz. 调用但文件未登记 ⇒ 新站点必须进清单。
	registered := map[string]bool{"pkg/authz/authz.go": true}
	for _, s := range authzSites {
		registered[s.File] = true
	}
	for _, ent := range authzRouteEntries {
		registered[ent.File] = true
	}
	for _, f := range sortedKeys(files) {
		if !strings.Contains(files[f], "authz.") {
			continue
		}
		if !registered[f] {
			probs = append(probs, fmt.Sprintf(
				"源文件 %s 调用了 pkg/authz 但未在 authzSites/authzRouteEntries 登记——新判定站点必须进清单（同步 PR 散点清单）", f))
		}
	}
	return probs
}

// ===== 守卫 =====

// TestAuthzEntryGuardCoverage 授权入口收口守卫（三层，见文件头）。
func TestAuthzEntryGuardCoverage(t *testing.T) {
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
	files := authzScanFiles(t)

	var probs []string
	// ① 矩阵 × 角色口径登记 × 承载标记
	probs = append(probs, authzCheckRoutes(authzRouteKinds(t), authzRouteEntries, rows, read)...)
	// ② 源码扫描 × 豁免清单
	probs = append(probs, authzScanBareJudgments(files, authzInlineExemptions)...)
	// ③ 判定站点/核心标记 + 反向对账
	probs = append(probs, authzCheckSites(files, read)...)

	for _, p := range probs {
		t.Error(p)
	}
	if len(probs) == 0 {
		t.Logf("授权收口守卫通过：%d 条路由、%d 个角色关口径登记、%d 个判定站点、%d 个豁免",
			len(rows), len(authzRouteEntries), len(authzSites), len(authzInlineExemptions))
	}
}

// TestAuthzEntryGuardNegativeControl 负对照（自证三层判定有牙齿）：
// 合成输入逐维破坏 ⇒ 对应层必报且点名；基准输入 ⇒ 零问题。
// 常态 CI 恒绿——它验证判定逻辑本身；真实文件级负对照见 PR 正文。
func TestAuthzEntryGuardNegativeControl(t *testing.T) {
	// 基准：一条 admin 路由（登记口径+承载标记齐全）
	baseKinds := map[string][]string{"GET /admin/x": {"AdminMiddleware"}}
	baseEntries := map[string]authzCarrier{
		"AdminMiddleware": {File: "carrier.go",
			Markers: []authzMarker{{Sub: "authz.IsAdminRole(", Min: 1}}},
	}
	baseRows := []routePolicy{
		{Method: "GET", Path: "/admin/x", Auth: "admin", Bucket: "global", Owner: "custom"},
	}
	baseRead := func(rel string) (string, error) {
		if rel == "carrier.go" {
			return "call authz.IsAdminRole( here", nil
		}
		b, err := os.ReadFile(filepath.Join("..", rel))
		return string(b), err
	}
	if probs := authzCheckRoutes(baseKinds, baseEntries, baseRows, baseRead); len(probs) != 0 {
		t.Fatalf("基准输入应零问题，实际: %v", probs)
	}

	// ①a 名单内角色关口径未登记（绕过执行点）
	kinds := map[string][]string{"GET /admin/x": {"RequireRole"}}
	if probs := authzCheckRoutes(kinds, map[string]authzCarrier{}, baseRows, baseRead); !hasProb(probs, "RequireRole") || !hasProb(probs, "未在 authzRouteEntries 登记") {
		t.Errorf("负对照①a失败：未登记角色关口径未被点名（probs=%v）", probs)
	}

	// ①b 矩阵 admin 但链上只有休眠口径（不算强制角色关）
	kinds = map[string][]string{"GET /admin/x": {"RequireAdmin"}}
	dormantEntries := map[string]authzCarrier{
		"RequireAdmin": {Dormant: true, File: "carrier.go",
			Markers: []authzMarker{{Sub: "authz.IsAdminRole(", Min: 1}}},
	}
	if probs := authzCheckRoutes(kinds, dormantEntries, baseRows, baseRead); !hasProb(probs, "没有登记的强制角色关") {
		t.Errorf("负对照①b失败：强制档缺角色关未被点名（probs=%v）", probs)
	}

	// ①c 承载文件删掉 authz 调用
	if probs := authzCheckRoutes(baseKinds, baseEntries, baseRows, func(rel string) (string, error) {
		return "no call here", nil
	}); !hasProb(probs, "缺少标记") || !hasProb(probs, "carrier.go") {
		t.Errorf("负对照①c失败：承载标记缺失未被点名（probs=%v）", probs)
	}

	// ①d 非休眠登记陈旧（口径没人用）
	entries := map[string]authzCarrier{
		"AdminMiddleware": baseEntries["AdminMiddleware"],
		"GhostRoleMw":     {File: "ghost.go", Markers: []authzMarker{{Sub: "x", Min: 1}}},
	}
	if probs := authzCheckRoutes(baseKinds, entries, baseRows, baseRead); !hasProb(probs, "登记陈旧") || !hasProb(probs, "GhostRoleMw") {
		t.Errorf("负对照①d失败：陈旧登记未被点名（probs=%v）", probs)
	}

	// ② 裸角色判定未豁免 / 裸归属判定未豁免 / 豁免陈旧
	files := map[string]string{"h.go": `if user.Role != "admin" { deny() }`}
	if probs := authzScanBareJudgments(files, map[string]string{}); !hasProb(probs, "裸授权判定") || !hasProb(probs, "h.go") {
		t.Errorf("负对照②a失败：裸角色判定未被点名（probs=%v）", probs)
	}
	ownFiles := map[string]string{"o.go": "if file.UserID == nil || *file.UserID != uid { deny() }"}
	if probs := authzScanBareJudgments(ownFiles, map[string]string{}); !hasProb(probs, "归属拒绝惯用式") && !hasProb(probs, "UserID解引用比较") {
		t.Errorf("负对照②b失败：裸归属判定未被点名（probs=%v）", probs)
	}
	if probs := authzScanBareJudgments(map[string]string{"h.go": "clean"}, map[string]string{"h.go": "历史理由"}); !hasProb(probs, "登记陈旧") {
		t.Errorf("负对照②c失败：陈旧豁免未被点名（probs=%v）", probs)
	}

	// ③ 站点标记缺失 / 反向未登记
	siteRead := func(rel string) (string, error) {
		if rel == "app/admin/auth.go" {
			return "no authz call", nil
		}
		b, err := os.ReadFile(filepath.Join("..", rel))
		return string(b), err
	}
	if probs := authzCheckSites(map[string]string{}, siteRead); !hasProb(probs, "缺少标记") || !hasProb(probs, "app/admin/auth.go") {
		t.Errorf("负对照③a失败：站点标记缺失未被点名（probs=%v）", probs)
	}
	if probs := authzCheckSites(map[string]string{"new/thing.go": "x := authz.IsAdminRole(r)"}, siteRead); !hasProb(probs, "未在 authzSites/authzRouteEntries 登记") {
		t.Errorf("负对照③b失败：未登记 authz 调用文件未被点名（probs=%v）", probs)
	}
}
