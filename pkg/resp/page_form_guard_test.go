// page_form_guard_test.go 分页成功响应形态守卫（core#124）。
//
// ## 为什么立这条
//
// 分页成功响应历史上有三套方言（口径表见本包 page_forms.go）：新增端点只能
// 挑一种抄 ⇒ 每次都有 1/3 概率抄出**第四种**。本守卫把「禁止第四种形态」变成
// 机械判据：凡响应载体含 total+page/page_size（结构体声明 / 字面量 / 手工
// map），或调用形态 A/B 的冻结构造器（resp.Page / respondLegacyPage），都必须
// 在下表登记（形态+位置+条数）。
//
// ## 判据（按**构造点/调用点**判定，core#8/#104 同款先例）
//
//	① decl    —— 带 json total+page(/page_size) 的结构体**类型声明**；
//	② literal —— 键含 Total+Page(/PageSize) 的结构体**字面量**；
//	③ map     —— 键含 "total"+"page"(/"page_size") 的手工 map 字面量；
//	④ call    —— resp.Page( / respondLegacyPage( 的**调用点**（A/B 冻结）。
//
// ⚠️ 判据是 AST 级（go/ast）：文本 grep 会把注释/字符串误判，也认不出
// pkg.Type{...} 的类型名。跳过 _test.go（测试里造同形结构是在验证行为）。
//
// ⚠️ 已知盲区：**非字面量**形态（先声明变量再逐字段赋值、unkeyed 字面量）
// 不在判据内——与 core#104 的「手写偏移」守卫同款取舍（判据求准不求全）。
//
// ## 棘轮
//
// 登记表按「文件+函数/类型名+条数」计数，**只许减不许增**：新增构造点必须
// 显式登记（含形态与理由），条目失效（该处已无对应构造）也判红 ⇒ 登记表
// 不会悄悄变成「什么都放行」的垃圾桶。
//
// ## 负对照
//
//	· TestPageFormGuardFlagsFourthForm：合成一个第四形态（decl+literal+map+call
//	  四种判据各一发）⇒ 守卫全数报红；
//	· TestPageFormRegistryRatchet：未登记 ⇒ 红；条目失效 ⇒ 红；计数漂移 ⇒ 红。
package resp

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// pageShapeHit 一处「分页同形载体/冻结构造器」的构造点或调用点。
type pageShapeHit struct {
	kind string // decl | literal | map | call
	file string // 模块根相对路径（斜杠分隔）
	name string // decl/literal: 类型名；map: "map"；call: 调用全名（如 resp.Page）
	fn   string // 所在函数名（类型声明为顶层 ⇒ 空串）
	line int
}

// pageFormAllowEntry 登记表条目（形态 + 位置 + 条数 + 理由）。
type pageFormAllowEntry struct {
	form   string // A | B | C | 豁免-MCP | 豁免-载体
	kind   string
	file   string
	name   string
	fn     string
	count  int
	reason string
}

// pageFormAllow 分页形态登记表（口径表 page_forms.go 的机械侧；**只许减**）。
//
// 三形态登记之外的条目都是「非 HTTP 分页信封」豁免（MCP structuredContent /
// app 内部载体），带理由登记，防止有人顺手把它们当 wire 信封直接吐出去。
var pageFormAllow = []pageFormAllowEntry{
	// ── 形态 A：resp.PageData（历史，冻结） ──
	{"A", "decl", "pkg/resp/response.go", "PageData", "", 1,
		"形态 A 登记类型（list,total,page,page_size）"},
	{"A", "literal", "pkg/resp/response.go", "PageData", "Page", 1,
		"形态 A 构造器 resp.Page 的唯一构造点"},
	{"A", "call", "gen/handler/admin/admin_manage.go", "resp.Page", "AdminListUsersFiltered", 1,
		"形态 A 唯一端点 /admin/users/filter；调用点冻结不得新增"},

	// ── 形态 B：respondLegacyPage（历史，冻结） ──
	{"B", "map", "gen/handler/admin/admin_manage.go", "map", "respondLegacyPage", 1,
		"形态 B 构造器的手工 map（全仓唯一允许的分页手工 map）"},
	{"B", "call", "gen/handler/admin/admin_manage.go", "respondLegacyPage", "AdminTransferLogs", 1,
		"形态 B 端点 /admin/logs/transfer；调用点冻结不得新增"},
	{"B", "call", "gen/handler/admin/admin_manage.go", "respondLegacyPage", "AdminListFilesFiltered", 1,
		"形态 B 端点 /admin/files/filter；调用点冻结不得新增"},

	// ── 形态 C：IDL 生成模型（新增端点唯一允许的形态） ──
	{"C", "literal", "gen/handler/admin/admin_service.go", "admin.AdminFileList", "AdminListFiles", 1,
		"形态 C：/admin/files（contracts gen/admin 模型）"},
	{"C", "literal", "gen/handler/admin/admin_service.go", "admin.AdminUserList", "AdminListUsers", 1,
		"形态 C：/admin/users（contracts gen/admin 模型）"},
	{"C", "literal", "gen/handler/admin/localfiles_ops.go", "admin.AdminActivitiesData", "AdminActivities", 1,
		"形态 C：/admin/activities（contracts gen/admin 模型）"},
	{"C", "literal", "gen/handler/share/share_service.go", "sharemodel.UserSharesListData", "UserSharesList", 1,
		"形态 C：/api/v1/user/shares（contracts gen/share 模型）"},
	{"C", "literal", "gen/handler/notify/notify_service.go", "notifymodel.MineData", "Mine", 1,
		"形态 C：/api/v1/notifies/mine（contracts gen/notify 模型）"},
	{"C", "literal", "gen/handler/notify/notify_service.go", "notifymodel.ListData", "toListData", 1,
		"形态 C：/admin/notifies（contracts gen/notify 模型 ListData）"},
	{"C", "literal", "app/user/service.go", "usermodel.UserFilePagination", "GetUserFiles", 1,
		"形态 C：/user/files 的 pagination 子结构（contracts gen/user 模型）"},

	// ── 豁免：MCP structuredContent（非 HTTP 响应信封） ──
	{"豁免-MCP", "decl", "app/mcp/tools.go", "listSharesOut", "", 1,
		"MCP 工具 structuredContent 输出契约（jsonschema），非 HTTP 分页信封"},
	{"豁免-MCP", "literal", "app/mcp/tools.go", "listSharesOut", "listShares", 1,
		"MCP list_shares 工具输出构造点"},
	{"豁免-MCP", "decl", "app/mcp/tools.go", "listUsersOut", "", 1,
		"MCP 工具 structuredContent 输出契约（jsonschema），非 HTTP 分页信封"},
	{"豁免-MCP", "literal", "app/mcp/tools.go", "listUsersOut", "listUsers", 1,
		"MCP list_users 工具输出构造点"},

	// ── 豁免：app 内部载体（wire 侧经 gen/handler/notify 重映射落形态 C） ──
	{"豁免-载体", "decl", "app/notify/notify.go", "ListData", "", 1,
		"notify service 层返回值；wire 侧经 toListData 重映射落形态 C（/admin/notifies）"},
	{"豁免-载体", "literal", "app/notify/notify.go", "ListData", "List", 1,
		"notify service 内部载体构造点"},
	{"豁免-载体", "decl", "app/notify/notify.go", "UserNotifyListData", "", 1,
		"notify service 层返回值；wire 侧经 Mine 重映射落形态 C"},
	{"豁免-载体", "literal", "app/notify/notify.go", "UserNotifyListData", "ListForUser", 1,
		"notify service 内部载体构造点"},
}

// TestPageFormNoFourthDialect 正判据：全仓的分页同形构造点/调用点全部已登记，
// 且登记条数与实际一致（棘轮）。
//
// 负对照（转红路径）：
//
//	· 在任一文件里新造第四形态（如 map{"total","page","page_size"} 直接上响应）
//	  ⇒ 「第四种分页响应形态」红；
//	· 给形态 A/B 新增调用点 ⇒ 未登记 call 命中红；
//	· 删掉登记表任一条目 ⇒ 该处命中无人登记红；登记条目失效 ⇒ 「条目失效」红。
func TestPageFormNoFourthDialect(t *testing.T) {
	root := pageFormRepoRoot(t)
	hits := collectPageShapeHits(t, root)
	viols := checkPageShapeRegistry(hits, pageFormAllow)
	if len(viols) > 0 {
		sort.Strings(viols)
		t.Errorf("分页成功响应形态守卫（core#124）发现 %d 处违例:\n%s\n"+
			"⇒ 口径表见 pkg/resp/page_forms.go：新增分页端点只允许形态 C"+
			"（contracts IDL 生成模型）；非 HTTP 载体须带理由登记。", len(viols), strings.Join(viols, "\n"))
	}
}

// TestPageFormGuardFlagsFourthForm 负对照：合成一个第四形态（四种判据各一发）
// ⇒ 守卫必须全数报红。守卫自身失效（判据变松/登记表放行一切）时本用例先红。
func TestPageFormGuardFlagsFourthForm(t *testing.T) {
	dir := t.TempDir()
	// 合成第四形态：结构体声明 + 字面量 + 手工 map + 冻结构造器调用，各一处。
	src := "package fourth\n\n" +
		"type fourthPage struct {\n" +
		"\tRows  []int  \x60json:\"rows\"\x60\n" +
		"\tTotal int64  \x60json:\"total\"\x60\n" +
		"\tPage  int    \x60json:\"page\"\x60\n" +
		"}\n\n" +
		"func build(c interface{}) interface{} {\n" +
		"\t_ = map[string]interface{}{\"total\": 1, \"page\": 1, \"page_size\": 20}\n" +
		"\t_ = fourthPage{Total: 1, Page: 1}\n" +
		"\tresp.Page(c, nil, 1, 1, 20)\n" +
		"\treturn nil\n" +
		"}\n"
	if err := os.WriteFile(filepath.Join(dir, "fourth.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("写合成文件失败: %v", err)
	}

	hits := collectPageShapeHits(t, dir)
	gotKinds := map[string]int{}
	for _, h := range hits {
		gotKinds[h.kind]++
	}
	for _, want := range []string{"decl", "literal", "map", "call"} {
		if gotKinds[want] != 1 {
			t.Errorf("合成第四形态应各命中一次 %s 判据，实际 %d（全部命中：%v）"+
				"⇒ 判据变松，第四形态能溜过去", want, gotKinds[want], gotKinds)
		}
	}

	// 用空登记表对账：合成根里的命中必须逐条报「第四种形态」
	// （真实登记表的条目失效判定与本负对照无关，由 TestPageFormRegistryRatchet 覆盖）。
	viols := checkPageShapeRegistry(hits, nil)
	if len(viols) != len(hits) {
		t.Errorf("合成第四形态的 %d 处命中应逐条报红，实际 %d 条违例:\n%s",
			len(hits), len(viols), strings.Join(viols, "\n"))
	}
	for _, v := range viols {
		if !strings.Contains(v, "fourth.go") {
			t.Errorf("违例应点名合成文件 fourth.go，实际: %s", v)
		}
	}
}

// TestPageFormRegistryRatchet 棘轮自检：登记表三种漂移方向都要转红。
func TestPageFormRegistryRatchet(t *testing.T) {
	hit := pageShapeHit{kind: "map", file: "x.go", name: "map", fn: "f", line: 1}
	entry := pageFormAllowEntry{form: "B", kind: "map", file: "x.go", name: "map", fn: "f", count: 1, reason: "r"}

	// 未登记 ⇒ 红
	if viols := checkPageShapeRegistry([]pageShapeHit{hit}, nil); len(viols) != 1 {
		t.Errorf("未登记的构造点应报 1 条违例，实际 %d", len(viols))
	}
	// 条目失效（登记 1 实际 0）⇒ 红
	if viols := checkPageShapeRegistry(nil, []pageFormAllowEntry{entry}); len(viols) != 1 {
		t.Errorf("失效条目应报 1 条违例，实际 %d", len(viols))
	}
	// 计数漂移（登记 1 实际 2）⇒ 红
	two := []pageShapeHit{hit, hit}
	if viols := checkPageShapeRegistry(two, []pageFormAllowEntry{entry}); len(viols) != 1 {
		t.Errorf("计数漂移应报 1 条违例，实际 %d", len(viols))
	}
	// 恰好登记 ⇒ 绿
	if viols := checkPageShapeRegistry([]pageShapeHit{hit}, []pageFormAllowEntry{entry}); len(viols) != 0 {
		t.Errorf("恰好登记不应有违例，实际 %v", viols)
	}
}

// checkPageShapeRegistry 对账「实际命中」与「登记表」，返回违例清单。
func checkPageShapeRegistry(hits []pageShapeHit, allow []pageFormAllowEntry) []string {
	matched := make([]int, len(allow))
	var viols []string
	for _, h := range hits {
		found := false
		for i, a := range allow {
			if a.kind == h.kind && a.file == h.file && a.name == h.name && a.fn == h.fn {
				matched[i]++
				found = true
				break
			}
		}
		if !found {
			viols = append(viols, fmt.Sprintf(
				"第四种分页响应形态: %s:%d（%s %s，函数 %s）—— 新增分页成功响应必须走口径表三形态之一"+
					"（首选形态 C：contracts IDL 生成模型），或在 pageFormAllow 登记非 HTTP 载体理由（core#124）",
				h.file, h.line, h.kind, h.name, h.fn))
		}
	}
	for i, a := range allow {
		if matched[i] == a.count {
			continue
		}
		if matched[i] == 0 {
			viols = append(viols, fmt.Sprintf(
				"登记条目失效: %s %s@%s（函数 %s）登记 %d 处、实际 0 处 —— 请删除条目（棘轮只许减）",
				a.kind, a.name, a.file, a.fn, a.count))
			continue
		}
		viols = append(viols, fmt.Sprintf(
			"登记条目计数漂移: %s %s@%s（函数 %s）登记 %d 处、实际 %d 处 —— 新增构造/调用点必须显式登记（含形态与理由），删除则下调计数",
			a.kind, a.name, a.file, a.fn, a.count, matched[i]))
	}
	return viols
}

// collectPageShapeHits 扫描 root 下全部生产 .go 文件（跳过 _test.go），提取
// 四种判据的命中。
func collectPageShapeHits(t *testing.T, root string) []pageShapeHit {
	t.Helper()
	var hits []pageShapeHit
	skipDirs := map[string]bool{".git": true, "bin": true, "data": true, "node_modules": true, "vendor": true, "testdata": true}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)

		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("解析 %s 失败: %v", rel, perr)
		}

		// 判据 ①：结构体类型声明（json total+page/page_size）。
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				if isPageShapeNames(jsonFieldNames(st), "total", "page", "page_size") {
					hits = append(hits, pageShapeHit{
						kind: "decl", file: rel, name: ts.Name.Name, fn: "",
						line: fset.Position(ts.Pos()).Line,
					})
				}
			}
		}

		// 判据 ②③④：字面量 / 手工 map / 冻结构造器调用。
		ast.Inspect(f, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CompositeLit:
				switch v.Type.(type) {
				case *ast.MapType:
					names := map[string]bool{}
					for _, elt := range v.Elts {
						if kv, ok := elt.(*ast.KeyValueExpr); ok {
							if s, ok := unquoteBasicLit(kv.Key); ok {
								names[s] = true
							}
						}
					}
					if isPageShapeNames(names, "total", "page", "page_size") {
						hits = append(hits, pageShapeHit{
							kind: "map", file: rel, name: "map",
							fn: enclosingFuncName(f, v), line: fset.Position(v.Pos()).Line,
						})
					}
				default:
					names := map[string]bool{}
					for _, elt := range v.Elts {
						if kv, ok := elt.(*ast.KeyValueExpr); ok {
							if id, ok := kv.Key.(*ast.Ident); ok {
								names[id.Name] = true
							}
						}
					}
					if isPageShapeNames(names, "Total", "Page", "PageSize") {
						hits = append(hits, pageShapeHit{
							kind: "literal", file: rel, name: typeExprString(v.Type),
							fn: enclosingFuncName(f, v), line: fset.Position(v.Pos()).Line,
						})
					}
				}
			case *ast.CallExpr:
				name := typeExprString(v.Fun)
				if name == "resp.Page" || name == "respondLegacyPage" {
					hits = append(hits, pageShapeHit{
						kind: "call", file: rel, name: name,
						fn: enclosingFuncName(f, v), line: fset.Position(v.Pos()).Line,
					})
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 %s 失败: %v", root, err)
	}
	return hits
}

// isPageShapeNames 判定字段/键名集合是否构成「分页同形」：含 total 且含
// page 或 page_size（Go 字段名与 json 键名两套拼写都走这一个判据）。
func isPageShapeNames(names map[string]bool, total, page, pageSize string) bool {
	if !names[total] {
		return false
	}
	return names[page] || names[pageSize]
}

// jsonFieldNames 提取结构体字段的 json 序列化名（去 omitempty 等选项）。
func jsonFieldNames(st *ast.StructType) map[string]bool {
	names := map[string]bool{}
	for _, field := range st.Fields.List {
		if field.Tag == nil {
			continue
		}
		tag, err := strconv.Unquote(field.Tag.Value)
		if err != nil {
			continue
		}
		name := strings.Split(reflect.StructTag(tag).Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		names[name] = true
	}
	return names
}

func unquoteBasicLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// typeExprString 渲染字面量/调用的类型或函数全名（pkg.Sel / 本包名）。
func typeExprString(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		if x := typeExprString(v.X); x != "" {
			return x + "." + v.Sel.Name
		}
		return v.Sel.Name
	case *ast.StarExpr:
		return typeExprString(v.X)
	default:
		return ""
	}
}

// enclosingFuncName 返回节点所在的最内层函数名（顶层为空串）。
func enclosingFuncName(f *ast.File, node ast.Node) string {
	name := ""
	ast.Inspect(f, func(n ast.Node) bool {
		if name != "" {
			return false
		}
		if fd, ok := n.(*ast.FuncDecl); ok && fd.Body != nil &&
			fd.Pos() <= node.Pos() && node.End() <= fd.End() {
			name = fd.Name.Name
			return false
		}
		return true
	})
	return name
}

// pageFormRepoRoot 从 go test 的 CWD（本包目录）向上定位模块根。
func pageFormRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			return root
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatal("向上未找到 go.mod，无法定位模块根")
		}
		root = parent
	}
}
