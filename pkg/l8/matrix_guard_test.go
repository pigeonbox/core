package l8

// matrix_guard_test.go —— L8 安全测试层矩阵的**双向对账守卫**。
//
// ## 为什么有这道门禁
//
// L8（安全层）此前是"零散用例"：取件码枚举钉桩、路径逃逸 reject、SSRF、认证中间件
// 都各自有测试，但**没有系统化的矩阵**——看不出「哪一类威胁有覆盖、哪一类没有」，
// 新增安全用例也不受任何机器约束（写不写、写在哪，全凭自觉）。
//
// 本守卫把「L8 是一层可审计的矩阵」变成机械判据：
//
//	① 清单结构：六个威胁类（固定集合）齐备、每类有覆盖条数下限、用例 id 唯一、
//	   每个用例至少挂一条测试、判定文案非空；
//	② 正向（清单 → 仓内真实测试）：清单里每条 tests 引用的文件必须存在，
//	   且该文件里**真的有**那个 Test 函数（AST 级解析，不是 grep 字符串）；
//	③ 反向（仓内安全测试 → 清单）：discovery globs 命中的每个安全测试文件必须
//	   被清单登记（被某个用例引用，或作为 helpers 登记且确实不含用例）；
//	   且这些文件里的**每个** Test 函数必须被某个用例认领，或被 exemptions
//	   显式豁免并写明理由——"写了没登记"与"登记了没写"都判红；
//	④ 豁免卫生：豁免条目必须真实存在、不得与用例重复认领、理由不得为空。
//
// ## 为什么与 bash 门禁分工
//
// 本守卫是**编译进 CI** 的一层（core CI 的 go test ./... 自动跑，零 workflow 改动）；
// scripts/check-l8-security-matrix.sh 是**入口层**（清单文件在场性 + 汇总 +
// --selfcheck 负对照）。两者读同一份清单 pkg/l8/matrix.json，判据不重复实现。
//
// ## 清单路径
//
// 默认 pkg/l8/matrix.json；可用环境变量 PB_L8_MATRIX 覆盖（门禁的 --selfcheck
// 用它注入坏形态清单做负对照）。

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// l8FixedClasses 六个固定威胁类（矩阵主键，缺一即红）。
var l8FixedClasses = []string{
	"auth_bypass",
	"authz",
	"ssrf",
	"pickup_enum",
	"path_escape",
	"resource_exhaustion",
}

// l8Manifest L8 矩阵清单（机器可读）。
type l8Manifest struct {
	SchemaVersion int           `json:"schema_version"`
	Layer         string        `json:"layer"`
	Classes       []l8Class     `json:"classes"`
	Discovery     l8Discovery   `json:"discovery"`
	Cases         []l8Case      `json:"cases"`
	Exemptions    []l8Exemption `json:"exemptions"`
	Helpers       []l8Helper    `json:"helpers"`
}

// l8Class 一个威胁类与其覆盖条数下限。
type l8Class struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	MinCases int    `json:"min_cases"`
}

// l8Discovery 「什么算 L8 安全测试文件」的识别规则。
type l8Discovery struct {
	Description string   `json:"description"`
	Globs       []string `json:"globs"`
}

// l8Case 矩阵的一行：一个威胁类 + 一个行为 + 证明它的测试。
type l8Case struct {
	ID      string   `json:"id"`
	Class   string   `json:"class"`
	Title   string   `json:"title"`
	Verdict string   `json:"verdict"`
	Tests   []string `json:"tests"`
}

// l8Exemption 语料里**不是**矩阵证据的测试函数（必须写理由）。
type l8Exemption struct {
	File   string `json:"file"`
	Func   string `json:"func"`
	Reason string `json:"reason"`
}

// l8Helper 不含独立用例的安全测试辅助文件（必须写理由，且确实不含用例）。
type l8Helper struct {
	File   string `json:"file"`
	Reason string `json:"reason"`
}

// l8ManifestPath 清单路径（默认 pkg/l8/matrix.json，可用 PB_L8_MATRIX 覆盖）。
func l8ManifestPath() string {
	if p := strings.TrimSpace(os.Getenv("PB_L8_MATRIX")); p != "" {
		return p
	}
	return "matrix.json"
}

// l8ModuleRoot 从 CWD 向上找 go.mod（模块根）。
func l8ModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("找不到模块根（go.mod）")
		}
		dir = parent
	}
}

// l8LoadManifest 读取并解析清单。
func l8LoadManifest(t *testing.T, manifestPath string) *l8Manifest {
	t.Helper()
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("读清单失败 %s: %v", manifestPath, err)
	}
	var m l8Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("清单不是合法 JSON %s: %v", manifestPath, err)
	}
	return &m
}

// l8GlobMatch 清单里的 glob 匹配（支持 "**/" 前缀与 "*"/"?" 通配）。
func l8GlobMatch(glob, rel string) bool {
	if strings.HasPrefix(glob, "**/") {
		rest := strings.TrimPrefix(glob, "**/")
		parts := strings.Split(rel, "/")
		for i := range parts {
			if ok, _ := path.Match(rest, strings.Join(parts[i:], "/")); ok {
				return true
			}
			if ok, _ := path.Match(rest, parts[i]); ok {
				return true
			}
		}
		return false
	}
	ok, _ := path.Match(glob, rel)
	return ok
}

// l8TestFuncs 解析一个 Go 文件，返回其中的 Test 函数名（不含 TestMain）。
//
// AST 级：文本 grep 会把注释/字符串里的 "func TestX(" 误判，也认不出变体形态。
func l8TestFuncs(t *testing.T, abs string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, abs, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("解析 Go 文件失败 %s: %v", abs, err)
	}
	out := map[string]bool{}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Recv != nil {
			continue
		}
		name := fd.Name.Name
		if !strings.HasPrefix(name, "Test") || name == "TestMain" {
			continue
		}
		out[name] = true
	}
	return out
}

// l8AllTestFiles 仓内全部 _test.go 文件（模块根相对路径）。
func l8AllTestFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(info.Name(), "_test.go") {
			rel, rerr := filepath.Rel(root, p)
			if rerr == nil {
				out = append(out, filepath.ToSlash(rel))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// TestL8MatrixGuard 矩阵清单 ↔ 仓内真实测试的**双向对账**。
func TestL8MatrixGuard(t *testing.T) {
	root := l8ModuleRoot(t)
	manifestPath := l8ManifestPath()
	if !filepath.IsAbs(manifestPath) {
		if _, err := os.Stat(manifestPath); err != nil {
			manifestPath = filepath.Join(root, manifestPath)
		}
	}
	m := l8LoadManifest(t, manifestPath)

	var problems []string
	add := func(format string, args ...interface{}) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	// ── ① 清单结构 ────────────────────────────────────────────────────────
	if m.SchemaVersion != 1 {
		add("清单 schema_version 必须是 1，实际 %d", m.SchemaVersion)
	}
	if m.Layer != "L8" {
		add("清单 layer 必须是 L8，实际 %q", m.Layer)
	}

	classIDs := map[string]int{}
	for _, c := range m.Classes {
		if _, dup := classIDs[c.ID]; dup {
			add("威胁类重复登记：%s", c.ID)
		}
		classIDs[c.ID] = c.MinCases
	}
	for _, want := range l8FixedClasses {
		if _, ok := classIDs[want]; !ok {
			add("威胁类缺失：%s（L8 的六个主键必须齐备）", want)
		}
	}
	for id := range classIDs {
		known := false
		for _, want := range l8FixedClasses {
			if id == want {
				known = true
				break
			}
		}
		if !known {
			add("未知威胁类：%s（只允许六个固定主键）", id)
		}
	}
	if len(m.Discovery.Globs) == 0 {
		add("discovery.globs 不能为空（否则反向对账形同虚设）")
	}

	caseIDs := map[string]bool{}
	classCases := map[string]int{}
	claimed := map[string]bool{} // "file::func"
	claimedFiles := map[string]bool{}
	for _, c := range m.Cases {
		if c.ID == "" {
			add("存在没有 id 的用例")
		}
		if caseIDs[c.ID] {
			add("用例 id 重复：%s", c.ID)
		}
		caseIDs[c.ID] = true
		if _, ok := classIDs[c.Class]; !ok {
			add("用例 %s 的威胁类不在六个主键内：%s", c.ID, c.Class)
		}
		classCases[c.Class]++
		if strings.TrimSpace(c.Title) == "" {
			add("用例 %s 缺标题", c.ID)
		}
		if strings.TrimSpace(c.Verdict) == "" {
			add("用例 %s 缺判定文案（verdict）", c.ID)
		}
		if len(c.Tests) == 0 {
			add("用例 %s 没有挂任何测试（清单里的每条必须有对应测试）", c.ID)
		}
		for _, raw := range c.Tests {
			file, funcName, ok := strings.Cut(raw, "::")
			if !ok || file == "" || funcName == "" {
				add("用例 %s 的测试项形态非法（应为 file::func）：%q", c.ID, raw)
				continue
			}
			if claimed[raw] {
				add("测试被重复认领：%s", raw)
			}
			claimed[raw] = true
			claimedFiles[file] = true
		}
	}

	// 覆盖条数下限（六个威胁类每类不低于阈值）
	for _, c := range m.Classes {
		if got := classCases[c.ID]; got < c.MinCases {
			add("威胁类 %s 的用例数 %d 低于下限 %d", c.ID, got, c.MinCases)
		}
	}

	// ── ② 正向：清单 → 仓内真实测试（AST 级）────────────────────────────
	for _, c := range m.Cases {
		for _, raw := range c.Tests {
			file, funcName, ok := strings.Cut(raw, "::")
			if !ok {
				continue
			}
			abs := filepath.Join(root, file)
			if _, err := os.Stat(abs); err != nil {
				add("用例 %s 引用的测试文件不存在：%s", c.ID, file)
				continue
			}
			funcs := l8TestFuncs(t, abs)
			if !funcs[funcName] {
				add("用例 %s 引用的测试函数不存在：%s::%s", c.ID, file, funcName)
			}
		}
	}

	// ── ③ 反向：仓内安全测试 → 清单 ─────────────────────────────────────
	helperFiles := map[string]bool{}
	for _, h := range m.Helpers {
		if helperFiles[h.File] {
			add("helpers 重复登记：%s", h.File)
		}
		helperFiles[h.File] = true
		if strings.TrimSpace(h.Reason) == "" {
			add("helpers 条目 %s 缺理由", h.File)
		}
	}

	exempted := map[string]bool{}
	for _, e := range m.Exemptions {
		key := e.File + "::" + e.Func
		if exempted[key] {
			add("豁免重复登记：%s", key)
		}
		exempted[key] = true
		if strings.TrimSpace(e.Reason) == "" {
			add("豁免条目 %s 缺理由（必须写明为什么不属六类威胁）", key)
		}
		if claimed[key] {
			add("豁免与用例重复认领同一测试：%s", key)
		}
		abs := filepath.Join(root, e.File)
		if _, err := os.Stat(abs); err != nil {
			add("豁免条目引用的文件不存在：%s", e.File)
			continue
		}
		funcs := l8TestFuncs(t, abs)
		if !funcs[e.Func] {
			add("豁免条目引用的测试函数不存在：%s", key)
		}
	}

	allFiles := l8AllTestFiles(t, root)
	corpusFiles := 0
	corpusFuncs := 0
	for _, rel := range allFiles {
		matched := false
		for _, g := range m.Discovery.Globs {
			if l8GlobMatch(g, rel) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		corpusFiles++
		funcs := l8TestFuncs(t, filepath.Join(root, rel))
		if len(funcs) == 0 {
			// 不含用例的辅助文件：必须显式登记为 helpers
			if !helperFiles[rel] {
				add("安全测试文件未登记进矩阵（且未登记为 helper）：%s", rel)
			}
			continue
		}
		// 文件级登记：被某个用例引用，或整份文件都被显式豁免（= 整体不属六类）。
		allExempted := true
		for fn := range funcs {
			if !exempted[rel+"::"+fn] {
				allExempted = false
				break
			}
		}
		if !claimedFiles[rel] && !allExempted {
			add("安全测试文件未登记进矩阵（没有任何用例引用它）：%s", rel)
		}
		for fn := range funcs {
			corpusFuncs++
			key := rel + "::" + fn
			if claimed[key] || exempted[key] {
				continue
			}
			add("仓内安全测试未登记进矩阵：%s", key)
		}
	}

	// helpers 里登记的文件若其实含用例 ⇒ 登记失真
	for file := range helperFiles {
		abs := filepath.Join(root, file)
		if _, err := os.Stat(abs); err != nil {
			add("helpers 登记的文件不存在：%s", file)
			continue
		}
		if funcs := l8TestFuncs(t, abs); len(funcs) > 0 {
			add("helpers 文件其实含用例（应作为用例登记）：%s", file)
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Errorf("L8 矩阵对账发现 %d 处问题：\n  %s", len(problems), strings.Join(problems, "\n  "))
		return
	}

	t.Logf("L8 矩阵对账通过：%d 个用例 / %d 个威胁类 / %d 个安全测试文件 / %d 个测试函数",
		len(m.Cases), len(m.Classes), corpusFiles, corpusFuncs)
	for _, id := range l8FixedClasses {
		t.Logf("  %-20s 用例 %2d 条（下限 %d）", id, classCases[id], classIDs[id])
	}
}
