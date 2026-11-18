package l6

// l6_matrix_guard_test.go —— L6 故障注入测试层矩阵的**双向对账**判定层。
//
// 为什么要有这一层（L6 此前是「零散用例」，没有覆盖真相源）
// ----------------------------------------------------------
// 故障注入用例此前散落在各域，每域各自造桩（miniredis Close、假存储、假
// operator），代价是：
//
//   · 同一种故障在不同域注入方式不一致，新域要重新发明一遍；
//   · 「注入 ⇒ 期望的降级/错误语义」只存在于用例注释里，**不可机器核对**；
//   · 没有覆盖真相源 ⇒ 看不出哪类故障有覆盖、哪类没有。
//
// ⇒ pkg/l6/matrix.json 是机器可读的覆盖真相源，本文件把它与**仓内真实测试**
// 双向对账（同构于 pkg/l8 的判定层）：
//
//	正向：清单里每条 case 都必须挂着仓内**真实存在**的测试（AST 级核对，
//	      不是字符串 grep）；清单里的 kind / 期望语义 / 业务码必须在受控词表内。
//	反向：discovery.globs 命中但**未登记进 cases** 的测试文件 ⇒ 判红
//	      （新加的故障注入用例不许游离在矩阵外）。
//
// 词表真相源在 internal/l6（Kind/Mode）；本包只能引用它们 ⇒ 写错类别名、
// 自造期望语义都跑不掉。typed_error 模式必须声明业务码，且该码必须在
// contracts/errcode 登记 ⇒ 跨仓语义漂移当场暴露。
//
// 环境变量：
//
//	PB_L6_MATRIX  清单路径（默认 matrix.json，相对 pkg/l6 或模块根）
//	PB_CONTRACTS  contracts 仓路径（默认取模块根的同级 ../contracts）
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
	"strconv"
	"strings"
	"testing"

	// 词表真相源在 internal/l6（core 仓内可见、外部不可 import）。
	// 包名与本包同名（都是 l6）⇒ 必须起别名。
	l6kit "github.com/pigeonbox/core/internal/l6"
)

// l6Manifest 清单（机器可读）。
type l6Manifest struct {
	SchemaVersion int            `json:"schema_version"`
	Layer         string         `json:"layer"`
	Kinds         []l6Kind       `json:"kinds"`
	Discovery     l6Discovery    `json:"discovery"`
	Cases         []l6Case       `json:"cases"`
	OutOfLayer    []l6OutOfLayer `json:"out_of_layer"`
	KnownGaps     []l6KnownGap   `json:"known_gaps"`
	Exemptions    []l6Exemption  `json:"exemptions"`
	Helpers       []l6Helper     `json:"helpers"`
}

// l6Kind 一个故障类型与其覆盖条数下限。
type l6Kind struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	MinCases int    `json:"min_cases"`
}

// l6Discovery 「什么算 L6 故障注入测试文件」的识别规则（路径 glob）。
type l6Discovery struct {
	Description string   `json:"description"`
	Globs       []string `json:"globs"`
}

// l6Expect 期望语义（受控词表 + 可选业务码）。
type l6Expect struct {
	Modes []l6kit.Mode `json:"modes"`
	// Code 期望的业务码（0 = 不要求具体码；typed_error 模式必须非 0）。
	Code int `json:"code"`
}

// l6Case 矩阵的一行：一个故障类型 + 注入点 + 必须成立的期望语义 + 证明它的测试。
type l6Case struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Point   string   `json:"point"`
	Title   string   `json:"title"`
	Verdict string   `json:"verdict"`
	Expect  l6Expect `json:"expect"`
	Tests   []string `json:"tests"`
}

// l6OutOfLayer 明确**不在本层**的故障（必须写理由与归属）。
type l6OutOfLayer struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Reason string `json:"reason"`
	Owner  string `json:"owner"`
}

// l6KnownGap 已知缺口（必须写理由与归属；不得凭空消失）。
type l6KnownGap struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Owner  string `json:"owner"`
}

// l6Exemption 语料里**不是**矩阵证据的测试函数（必须写理由）。
type l6Exemption struct {
	File   string `json:"file"`
	Func   string `json:"func"`
	Reason string `json:"reason"`
}

// l6Helper 不含独立用例的故障注入辅助文件（必须写理由）。
type l6Helper struct {
	File   string `json:"file"`
	Reason string `json:"reason"`
}

// l6ManifestPath 清单路径（默认 matrix.json，可用 PB_L6_MATRIX 覆盖）。
func l6ManifestPath() string {
	if p := strings.TrimSpace(os.Getenv("PB_L6_MATRIX")); p != "" {
		return p
	}
	return "matrix.json"
}

// l6ModuleRoot 从 CWD 向上找 go.mod（模块根）。
func l6ModuleRoot(t *testing.T) string {
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

// l6LoadManifest 读取并解析清单。
func l6LoadManifest(t *testing.T, manifestPath string) *l6Manifest {
	t.Helper()
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("读清单失败 %s: %v", manifestPath, err)
	}
	var m l6Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("清单不是合法 JSON %s: %v", manifestPath, err)
	}
	return &m
}

// l6GlobMatch 清单里的 glob 匹配（支持 "**/" 前缀与 "*"/"?" 通配）。
func l6GlobMatch(glob, rel string) bool {
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

// l6TestFuncs 解析一个 Go 文件，返回其中的 Test 函数名（不含 TestMain）。
//
// AST 级：文本 grep 会把注释/字符串里的 "func TestX(" 误判，也认不出变体形态。
func l6TestFuncs(t *testing.T, abs string) map[string]bool {
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

// l6AllTestFiles 仓内全部 _test.go 文件（模块根相对路径）。
func l6AllTestFiles(t *testing.T, root string) []string {
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

// l6KindKnown 该故障类型是否在受控词表内（真相源 internal/l6kit.AllKinds）。
func l6KindKnown(id string) bool {
	for _, k := range l6kit.AllKinds {
		if string(k) == id {
			return true
		}
	}
	return false
}

// l6ModeKnown 该期望语义是否在受控词表内（真相源 internal/l6kit.AllModes）。
func l6ModeKnown(m l6kit.Mode) bool {
	for _, known := range l6kit.AllModes {
		if known == m {
			return true
		}
	}
	return false
}

// l6ContractsErrCodes 读 contracts 仓的 errcode 常量表，返回「码 → 常量名」。
//
// 找不到 contracts 仓时返回空 map（非 nil）——判定层据此把 typed_error 的
// 码核对降级为「跳过并说明」，而不是假装通过（判定面不落在本仓时不可判）。
func l6ContractsErrCodes(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	root := l6ModuleRoot(t)
	contractsDir := strings.TrimSpace(os.Getenv("PB_CONTRACTS"))
	if contractsDir == "" {
		contractsDir = filepath.Join(filepath.Dir(root), "contracts")
	}
	rel := filepath.Join(contractsDir, "errcode", "errcode.go")
	raw, err := os.ReadFile(rel)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "Code") {
			continue
		}
		name, rest, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		code := strings.TrimSpace(rest)
		if i := strings.Index(code, "//"); i >= 0 {
			code = strings.TrimSpace(code[:i])
		}
		if code == "" || name == "" {
			continue
		}
		out[code] = name
	}
	return out
}

// TestL6MatrixGuard 矩阵清单 ↔ 仓内真实测试的**双向对账**。
func TestL6MatrixGuard(t *testing.T) {
	root := l6ModuleRoot(t)
	manifestPath := l6ManifestPath()
	if !filepath.IsAbs(manifestPath) {
		if _, err := os.Stat(manifestPath); err != nil {
			manifestPath = filepath.Join(root, manifestPath)
		}
	}
	m := l6LoadManifest(t, manifestPath)

	var problems []string
	add := func(format string, args ...interface{}) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	// ── 词表封闭：清单里的 kind 必须在受控词表内 ─────────────────────────
	kindSet := map[string]bool{}
	for _, k := range m.Kinds {
		kindSet[k.ID] = true
		if !l6KindKnown(k.ID) {
			add("kinds 里的 %q 不在 internal/l6 的 Kind 词表内（写错类别名或自造类别）", k.ID)
		}
	}

	// ── 正向：每条 case 的 tests 必须真实存在 ───────────────────────────
	registeredFiles := map[string]bool{}
	caseCountByKind := map[string]int{}

	for _, c := range m.Cases {
		if c.ID == "" {
			add("有 case 缺 id")
		}
		if c.Kind == "" {
			add("%s: case 缺 kind（故障类型必填）", c.ID)
		} else {
			if !kindSet[c.Kind] {
				add("%s: kind %q 不在清单声明的 kinds 内", c.ID, c.Kind)
			}
			if !l6KindKnown(c.Kind) {
				add("%s: kind %q 不在 internal/l6 的 Kind 词表内", c.ID, c.Kind)
			}
			caseCountByKind[c.Kind]++
		}
		if c.Title == "" {
			add("%s: case 缺 title（这条用例在验什么必须写清）", c.ID)
		}
		if c.Verdict == "" {
			add("%s: case 缺 verdict（期望语义必须成文，不能只存在于注释里）", c.ID)
		}
		if len(c.Tests) == 0 {
			add("%s: case 没有登记任何测试（清单里的每条都必须有仓内真实测试兜着）", c.ID)
		}
		if len(c.Expect.Modes) == 0 {
			add("%s: case 缺 expect.modes（期望语义必填）", c.ID)
		}
		hasTypedError := false
		for _, mode := range c.Expect.Modes {
			if !l6ModeKnown(mode) {
				add("%s: 期望语义 %q 不在 internal/l6 的 Mode 词表内（自造语义跑不掉）", c.ID, string(mode))
			}
			if mode == l6kit.ModeTypedError {
				hasTypedError = true
			}
		}
		// typed_error 必须声明业务码
		if hasTypedError && c.Expect.Code == 0 {
			add("%s: 期望语义含 typed_error ⇒ 必须声明具体业务码", c.ID)
		}
		// 非 typed_error 不该带码（避免「码与语义错配」藏起来）
		if !hasTypedError && c.Expect.Code != 0 {
			add("%s: 期望语义不含 typed_error 却声明了业务码 %d（码与语义错配）", c.ID, c.Expect.Code)
		}

		for _, ref := range c.Tests {
			file, funcName, ok := strings.Cut(ref, "::")
			if !ok {
				add("%s: 测试引用 %q 形如「文件::函数」", c.ID, ref)
				continue
			}
			registeredFiles[file] = true
			abs := filepath.Join(root, file)
			if _, err := os.Stat(abs); err != nil {
				add("%s: 登记的测试文件不存在 %s", c.ID, file)
				continue
			}
			funcs := l6TestFuncs(t, abs)
			if !funcs[funcName] {
				add("%s: 登记的测试 %s::%s 在文件里不存在（清单与真实测试脱节）", c.ID, file, funcName)
			}
		}
	}

	// ── 每类故障的覆盖条数下限 ─────────────────────────────────────────
	for _, k := range m.Kinds {
		if got := caseCountByKind[k.ID]; got < k.MinCases {
			add("kind %q 的覆盖条数 %d 低于声明的下限 %d（某类故障悄悄没覆盖）", k.ID, got, k.MinCases)
		}
	}

	// ── typed_error 的业务码必须在 contracts/errcode 登记 ───────────────
	errCodes := l6ContractsErrCodes(t)
	for _, c := range m.Cases {
		if c.Expect.Code == 0 {
			continue
		}
		if len(errCodes) == 0 {
			t.Logf("提示：未找到 contracts/errcode/errcode.go ⇒ 本次跳过 typed_error 业务码核对（判定面不在本仓）")
			break
		}
		if _, ok := errCodes[strconv.Itoa(c.Expect.Code)]; !ok {
			add("%s: 期望的业务码 %d 未在 contracts/errcode 登记（跨仓语义漂移）", c.ID, c.Expect.Code)
		}
	}

	// ── 反向：globs 命中但未登记进 cases 的测试文件 ⇒ 判红 ──────────────
	exemptFiles := map[string]bool{}
	for _, e := range m.Exemptions {
		if e.File == "" || e.Reason == "" {
			add("有条 exemption 缺 file 或 reason（豁免必须写理由）")
		}
		exemptFiles[e.File] = true
	}
	helperFiles := map[string]bool{}
	for _, h := range m.Helpers {
		if h.File == "" || h.Reason == "" {
			add("有条 helper 缺 file 或 reason（辅助文件必须写理由）")
		}
		helperFiles[h.File] = true
	}

	for _, file := range l6AllTestFiles(t, root) {
		hit := false
		for _, glob := range m.Discovery.Globs {
			if l6GlobMatch(glob, file) {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		if registeredFiles[file] || exemptFiles[file] || helperFiles[file] {
			continue
		}
		add("故障注入测试文件 %s 命中 discovery.globs 但未登记进 cases（也不在 exemptions/helpers 里）⇒ 新用例游离在矩阵外", file)
	}

	// ── known_gaps / out_of_layer 必须写齐归属 ─────────────────────────
	for _, g := range m.KnownGaps {
		if g.ID == "" || g.Title == "" || g.Detail == "" || g.Owner == "" {
			add("有条 known_gap 缺 id/title/detail/owner（已知缺口必须写清归属，不得凭空消失）")
		}
	}
	for _, o := range m.OutOfLayer {
		if o.ID == "" || o.Title == "" || o.Reason == "" || o.Owner == "" {
			add("有条 out_of_layer 缺 id/title/reason/owner（层外故障必须写清为什么与归属）")
		}
	}

	if len(problems) > 0 {
		for _, p := range problems {
			t.Errorf("L6 矩阵对账失败: %s", p)
		}
		t.Fatalf("L6 矩阵与仓内真实测试不一致（%d 项）", len(problems))
	}
	t.Logf("L6 矩阵对账通过：%d 条用例 · %d 类故障 · %d 个故障注入测试文件", len(m.Cases), len(m.Kinds), len(registeredFiles))
}
