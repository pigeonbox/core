// optional_deref_guard_test.go 契约可选字段解引用守卫（2026-10-25 实录事故引入）。
//
// 事故：gen/handler/notify.List 里 `int(*req.Page)` 直解 thrift **optional** 指针
// ⇒ 客户端不带 page/page_size 时 nil pointer dereference **panic**
// ⇒ 管理台「公告管理」页整个 500（生产端被 Recovery 兜成 500，故障是「一进页面就红」）。
// 该域此前 0% 测试覆盖 ⇒ 无人发现。
//
// 本守卫把这一类缺陷变成**机械可查**：
//
//	① 扫 contracts/gen/**  ⇒ 得到「结构体 → optional 指针字段」表；
//	② 扫 core 的 handler 层 ⇒ 找 `*var.Field` 解引用（var 的类型是契约结构体）；
//	③ 每个解引用必须在**同一函数**里有显式保护：
//	   - `if var.Field != nil` / `== nil` 的比较；
//	   - `var.IsSetField()` 调用（thrift 生成的判空器）；
//	   - 或改用契约生成的 `var.GetField()` 访问器（未设置时返回契约默认值，永不 panic）。
//
// 判据（守卫能不能变红）：
//
//	· 把 notify.List 改回 `int(*req.Page)` ⇒ 本守卫红；
//	· 把某个 `*req.X` 的 nil 守卫删掉 ⇒ 本守卫红；
//	· 若解析口径坏了（optional 表为空 / 一个解引用都扫不到）⇒ 守卫**也红**
//	  （「守卫看起来存在但实际没跑」是最坏的形态，见 AGENTS.md 门禁失效形态）。
//
// 刻意**不做**的事：不做完整类型检查（不引 x/tools，避免给 core 加重量级依赖）；
// 只在「局部变量由契约结构体字面量声明」这一种（生成代码里的主流写法）上判定。
// 漏判方向是「宁可放过、不可误伤」——误报会让正常代码被要求加冗余守卫。
package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// contractsModulePath contracts 的 Go module 路径（optional 字段真相源）。
const contractsModulePath = "github.com/pigeonbox/contracts"

// contractsImportPrefix handler 层引用契约包的路径前缀。
const contractsImportPrefix = "github.com/pigeonbox/contracts/gen/"

// optionalFields 「结构体名 → optional 指针字段名列表」。
type optionalFields map[string][]string

// loadOptionalFields 解析 contracts/gen/**，得到「结构体 → optional 指针字段」表。
//
// 契约侧真相源 = thrift 生成代码的 struct tag（`thrift:"page,1,optional"`）
// ⇒ optional 一律是**指针**（thriftgo 生成规则）；required 是值类型。
func loadOptionalFields(t *testing.T, contractsDir string) optionalFields {
	t.Helper()
	out := optionalFields{}
	genDir := filepath.Join(contractsDir, "gen")
	if _, err := os.Stat(genDir); err != nil {
		t.Fatalf("contracts 生成目录不存在: %v（genDir=%s）", err, genDir)
	}
	err := filepath.WalkDir(genDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
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
				for _, field := range st.Fields.List {
					if len(field.Names) != 1 || field.Tag == nil {
						continue
					}
					// optional ⇒ 指针（thriftgo 规则）；非指针的 optional 忽略。
					if _, isPtr := field.Type.(*ast.StarExpr); !isPtr {
						continue
					}
					tagText := strings.Trim(field.Tag.Value, "`")
					if !strings.Contains(tagText, "optional") {
						continue
					}
					out[ts.Name.Name] = append(out[ts.Name.Name], field.Names[0].Name)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("解析 contracts 生成代码失败: %v", err)
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

// contractsSourceDir 定位 contracts 源码目录。
//
// 两种布局都要支持：
//
//	· 工作区（go.work）：../contracts（与 core 同级）；
//	· 独立检出（CI）：go list 给出模块缓存路径。
//
// 找不到 ⇒ **响亮失败**（不静默跳过）：守卫「看起来存在但实际没跑」是最坏的形态。
func contractsSourceDir(t *testing.T) string {
	t.Helper()
	// ① go list（工作区/模块缓存都对）
	if out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", contractsModulePath).Output(); err == nil {
		dir := strings.TrimSpace(string(out))
		if dir != "" {
			if _, serr := os.Stat(filepath.Join(dir, "gen")); serr == nil {
				return dir
			}
		}
	}
	// ② 工作区相对路径兜底
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("取工作目录失败: %v", err)
	}
	for i := 0; i < 4; i++ {
		cand := filepath.Join(wd, "..", "contracts")
		if _, serr := os.Stat(filepath.Join(cand, "gen")); serr == nil {
			return filepath.Clean(cand)
		}
		wd = filepath.Dir(wd)
	}
	t.Fatalf("定位 contracts 源码失败：go list 无输出且工作区相对路径也找不到（%s）", contractsModulePath)
	return ""
}

// derefSite 一个被检出的「optional 指针解引用」位置。
type derefSite struct {
	File  string
	Line  int
	Func  string
	Var   string
	Field string
	Text  string
}

// TestNoUnguardedOptionalDeref handler 层不得直解契约 optional 指针而无保护。
//
// 负对照：notify.List 改回 `int(*req.Page)` ⇒ 本用例红。
func TestNoUnguardedOptionalDeref(t *testing.T) {
	optional := loadOptionalFields(t, contractsSourceDir(t))
	if len(optional) == 0 {
		t.Fatal("契约 optional 字段表为空——解析口径坏了（守卫会形同虚设）")
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("取工作目录失败: %v", err)
	}
	coreRoot := filepath.Dir(filepath.Dir(wd)) // core/internal/arch → core

	var sites []derefSite
	scanned := 0
	totalDerefs := 0
	for _, rel := range []string{"gen/handler", "transport/http/handler"} {
		base := filepath.Join(coreRoot, rel)
		if _, serr := os.Stat(base); serr != nil {
			continue
		}
		found, n, derefs, ferr := scanHandlerDir(base, coreRoot, optional)
		if ferr != nil {
			t.Fatalf("扫描 %s 失败: %v", rel, ferr)
		}
		sites = append(sites, found...)
		scanned += n
		totalDerefs += derefs
	}
	if scanned == 0 {
		t.Fatal("一个 handler 文件都没扫到——扫描口径坏了（守卫会形同虚设）")
	}
	// 自检：必须**扫到过** optional 指针解引用（哪怕都已加保护），
	// 否则说明「扫描口径」坏了 ⇒ 守卫在放行一切 ⇒ 形同虚设。
	if totalDerefs == 0 {
		t.Fatal("一个 optional 指针解引用都没扫到——扫描口径坏了（守卫会形同虚设）")
	}

	sort.Slice(sites, func(i, j int) bool {
		if sites[i].File != sites[j].File {
			return sites[i].File < sites[j].File
		}
		return sites[i].Line < sites[j].Line
	})
	for _, s := range sites {
		t.Errorf("未加保护的 optional 指针解引用：%s:%d（%s 内 `*%s.%s`）\n\t%s\n\t⇒ 客户端不传该可选字段时 nil deref panic；修法：加 `!= nil` 守卫 / 用 `IsSet%s()` / 改用 `Get%s()` 访问器",
			s.File, s.Line, s.Func, s.Var, s.Field, s.Text, s.Field, s.Field)
	}
}

// scanHandlerDir 扫描一个 handler 目录，找出「无保护的 optional 指针解引用」。
// 返回：违规点、扫到的文件数。
func scanHandlerDir(dir, coreRoot string, optional optionalFields) ([]derefSite, int, int, error) {
	var out []derefSite
	files := 0
	totalDerefs := 0
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		files++

		// import 别名 → 契约域（notify / share / …）
		aliasDomain := map[string]string{}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if !strings.HasPrefix(p, contractsImportPrefix) {
				continue
			}
			domain := strings.TrimPrefix(p, contractsImportPrefix)
			if i := strings.IndexAny(domain, "/"); i >= 0 {
				domain = domain[:i]
			}
			alias := filepath.Base(p)
			if imp.Name != nil {
				alias = imp.Name.Name
			}
			aliasDomain[alias] = domain
		}
		if len(aliasDomain) == 0 {
			return nil
		}

		rel, _ := filepath.Rel(coreRoot, path)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			found, n := scanFunc(fset, rel, fd, aliasDomain, optional)
			out = append(out, found...)
			totalDerefs += n
		}
		return nil
	})
	return out, files, totalDerefs, err
}

// scanFunc 扫描单个函数：建立 local var → 契约结构体的映射，收集保护证据，再判定解引用。
func scanFunc(fset *token.FileSet, rel string, fd *ast.FuncDecl,
	aliasDomain map[string]string, optional optionalFields) ([]derefSite, int) {

	varStruct := map[string]string{}  // 局部变量名 → 契约结构体名
	guarded := map[string]bool{}      // "var.Field" → 有保护

	// ① 变量绑定 + 保护证据（先跑一遍收集，再跑一遍判定，避免顺序依赖）
	collect := func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range v.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok {
					continue
				}
				for _, rhs := range v.Rhs {
					if s := structNameOf(rhs, aliasDomain); s != "" {
						varStruct[id.Name] = s
					}
				}
			}
		case *ast.ValueSpec:
			for _, id := range v.Names {
				if v.Type == nil {
					continue
				}
				if s := structNameOfType(v.Type, aliasDomain); s != "" {
					varStruct[id.Name] = s
				}
			}
		case *ast.IfStmt:
			collectNilGuards(v.Cond, varStruct, optional, guarded)
		case *ast.BinaryExpr:
			collectNilGuards(v, varStruct, optional, guarded)
		case *ast.CallExpr:
			sel, ok := v.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if _, known := varStruct[id.Name]; known && strings.HasPrefix(sel.Sel.Name, "IsSet") {
				guarded[id.Name+"."+strings.TrimPrefix(sel.Sel.Name, "IsSet")] = true
			}
		}
		return true
	}
	ast.Inspect(fd.Body, collect)

	// ② 解引用点判定
	var sites []derefSite
	total := 0
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		// ⚠️ Go AST 里表达式中的 `*p` 是 *ast.StarExpr（不是 UnaryExpr{MUL}；
		// 后者只在类型上下文出现）。两者都收，避免漏判。
		var star *ast.StarExpr
		switch v := n.(type) {
		case *ast.StarExpr:
			star = v
		case *ast.UnaryExpr:
			if v.Op == token.MUL {
				star = &ast.StarExpr{X: v.X}
			}
		}
		if star == nil {
			return true
		}
		sel, ok := star.X.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		st, known := varStruct[id.Name]
		if !known {
			return true
		}
		fields, isContract := optional[st]
		if !isContract {
			return true
		}
		for _, fld := range fields {
			if fld != sel.Sel.Name {
				continue
			}
			total++
			if guarded[id.Name+"."+fld] {
				return true
			}
			pos := fset.Position(star.Pos())
			sites = append(sites, derefSite{
				File:  rel,
				Line:  pos.Line,
				Func:  fd.Name.Name,
				Var:   id.Name,
				Field: fld,
				Text:  exprText(star),
			})
			return true
		}
		return true
	})
	return sites, total
}

// structNameOf 从赋值右侧推断契约结构体名（`x := notifymodel.Foo{}`）。
func structNameOf(rhs ast.Expr, aliasDomain map[string]string) string {
	switch v := rhs.(type) {
	case *ast.CompositeLit:
		return structNameOfType(v.Type, aliasDomain)
	case *ast.UnaryExpr:
		if cl, ok := v.X.(*ast.CompositeLit); ok {
			return structNameOfType(cl.Type, aliasDomain)
		}
	case *ast.CallExpr:
		if cl, ok := v.Fun.(*ast.CompositeLit); ok {
			return structNameOfType(cl.Type, aliasDomain)
		}
	}
	return ""
}

// structNameOfType 从类型表达式取结构体名（仅认「契约包别名.结构体」形态）。
func structNameOfType(t ast.Expr, aliasDomain map[string]string) string {
	switch v := t.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		pkg, ok := v.X.(*ast.Ident)
		if !ok {
			return ""
		}
		if _, isContractPkg := aliasDomain[pkg.Name]; !isContractPkg {
			return ""
		}
		return v.Sel.Name
	case *ast.StarExpr:
		return structNameOfType(v.X, aliasDomain)
	}
	return ""
}

// collectNilGuards 在条件表达式里收集「nil 比较」保护证据。
func collectNilGuards(cond ast.Expr, varStruct map[string]string, optional optionalFields, guarded map[string]bool) {
	bin, ok := cond.(*ast.BinaryExpr)
	if !ok {
		return
	}
	if bin.Op != token.EQL && bin.Op != token.NEQ {
		return
	}
	var sel *ast.SelectorExpr
	var other ast.Expr
	if s, ok := bin.X.(*ast.SelectorExpr); ok {
		sel, other = s, bin.Y
	} else if s, ok := bin.Y.(*ast.SelectorExpr); ok {
		sel, other = s, bin.X
	} else {
		return
	}
	if !isNilExpr(other) {
		return
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return
	}
	st, known := varStruct[id.Name]
	if !known {
		return
	}
	if _, isContract := optional[st]; !isContract {
		return
	}
	guarded[id.Name+"."+sel.Sel.Name] = true
}

// isNilExpr 判定表达式是否为 nil 字面量（go/ast 无 ast.NilLit，用 Ident 判）。
func isNilExpr(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}

// exprText 尽力渲染表达式文本（用于报错信息）。
func exprText(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.UnaryExpr:
		return v.Op.String() + exprText(v.X)
	case *ast.SelectorExpr:
		return exprText(v.X) + "." + v.Sel.Name
	case *ast.Ident:
		return v.Name
	case *ast.StarExpr:
		return "*" + exprText(v.X)
	case *ast.BinaryExpr:
		return exprText(v.X) + " " + v.Op.String() + " " + exprText(v.Y)
	}
	return "?"
}
