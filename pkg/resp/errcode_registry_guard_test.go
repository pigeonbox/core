// errcode_registry_guard_test.go 业务码登记守卫（core 侧兜底，L2 层）
//
// ## 为什么立这条守卫
//
// 业务码的**真相源**在 contracts/errcode（手写常量表 CodeXxx = NNNNN + 一张
// messages 文案表）。core 全仓 96 个文件 import 它，但**没有任何机制**保证
// core 源码里出现的码都在契约登记清单内：多处以**字面量**把 5 位码直接喂给
// resp.NewError* / pkg/errors 构造器（gen/handler/admin/admin_manage.go、
// transport/http/handler/helpers.go、transport/http/handler/preview_thumb.go、
// gen/handler/ratelimit/ratelimit_service.go、gen/handler/preview 等约 29 处）。
// 今天它们恰好都在清单内 ⇒ **纯属约定**——契约一旦删码/改码，core 侧不会有人
// 被告知，直到线上出现一个「契约里根本没有的业务码」。
//
// 本守卫扫描的是 **core 自己的源码**（go/parser + go/ast，不用正则）⇒
// 不受 import 别名（bootstrap/auto_backup.go 的 contracsErrcode、
// app/* 的 pkgerrors）与换行/括号写法影响。
//
// ## 与 hub 侧跨仓门禁的关系：互补，不是重复
//
//   - hub 侧门禁管「三端（server / frontend / desktop…）的错误码集合**相等**」
//     （跨仓、一致性）；
//   - 本守卫管「**core 源码里的字面量不越界**」（单仓、内部）。
//     两者看的对象不同（集合相等 vs 单仓内越界），任一都不能替代另一个：
//     hub 门禁绿 ≠ core 源码没写死未登记码；本守卫绿 ≠ 三端集合一致。
//
// ## 三个方向（双向）
//
//	① core 用了**未登记**的码        ⇒ 红（点名「文件:行 + 码值」）
//	② 契约登记码解析不出**非兜底**文案 ⇒ 红（errcode.Message 落兜底文案）
//	③ 契约登记码没有 **HTTP 状态映射**  ⇒ 红（落 default ⇒ HTTP 200 ⇒ 业务错误进 200）
//
// ## 负对照（如何让它变红）
//
//	① 在 transport/http/handler/helpers.go:58 把字面量 10001 改成 19999（未登记）
//	   ⇒ TestErrcodeRegistry_CoreLiteralCodesAreRegistered 红，输出点名
//	     "transport/http/handler/helpers.go:58: 码 19999 未在 contracts/errcode 登记"。
//	② 在 contracts 新增一个登记码（并给出文案）但不动 core 的 httpStatusForCode
//	   ⇒ 该码落 default ⇒ HTTP 200 ⇒ TestErrcodeRegistry_HTTPStatusMappingCoversContractCodes 红。
//	③ 把 pkg/resp/response.go 里某个**已在交付面使用**的码（如 CodeConflict 10016）
//	   从 httpStatusForCode 的 switch 中删掉 ⇒ 该码落 default ⇒ 200 ⇒ 同上用例红。
//
// ## 扫描器的正对照
//
// 守卫自身失效（import 解析错、构造器改名、扫描范围被改窄）时，「没有越界码」
// 会**假绿**。⇒ TestErrcodeRegistry_ScannerPositiveControl 钉死「必须扫到哪些
// 已知的交付码」+「字面量点位的下限」，扫不到即红。
package resp

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/pigeonbox/contracts/errcode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// coreRespPkgPath core 自己的 resp 包（错误响应出口）。
	coreRespPkgPath = "github.com/pigeonbox/core/pkg/resp"
	// coreErrorsPkgPath core 自己的 errors 包（业务错误构造器）。
	coreErrorsPkgPath = "github.com/pigeonbox/core/pkg/errors"
	// contractsErrcodePkg 契约的错误码登记**包**路径（源码里的 import 路径）。
	contractsErrcodePkg = "github.com/pigeonbox/contracts/errcode"
	// contractsModulePath 契约的**模块**路径（模块缓存目录名用它）。
	contractsModulePath = "github.com/pigeonbox/contracts"
	// errCodeMin / errCodeMax 5 位业务码的值域（10000..99999）。
	errCodeMin = 10000
	errCodeMax = 99999
)

// =====================================================================
// 基础设施
// =====================================================================

// coreModuleRoot 定位 core 模块根（自本测试文件所在目录逐级上溯找 go.mod）。
//
// 不用相对路径 "../../"：测试的工作目录是**包目录**，但一旦有人把文件挪到
// 别的层级，相对路径就指错地方 ⇒ 守卫会扫到空目录 ⇒ 假绿。
func coreModuleRoot(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	require.True(t, ok, "无法定位测试源文件路径")
	dir := filepath.Dir(self)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("自 %s 上溯未找到 go.mod（core 模块根定位失败）", self)
		}
		dir = parent
	}
}

// contractsErrcodeSource 定位 contracts/errcode 的源码文件（登记真相源）。
//
// 为什么不写死模块缓存路径：缓存目录随 GOPATH/GOMODCACHE 变，本机与 CI 不同。
// 取数顺序：
//
//	① go list -m -f {{.Dir}}（权威，含 replace 场景）；
//	② 兜底：GOMODCACHE + 版本号（版本号取自 debug.ReadBuildInfo 的依赖表，
//	   不额外起子进程）。
func contractsErrcodeSource(t *testing.T) string {
	t.Helper()
	if p, ok := contractsErrcodeSourceByBuildInfo(t); ok {
		return p
	}
	if p, ok := contractsErrcodeSourceByGoList(t); ok {
		return p
	}
	t.Fatalf("定位 contracts/errcode 源码失败：build-info 与 go-list 两条路径都没命中")
	return ""
}

// contractsErrcodeSourceByGoList 用 go list -m 问出模块实际落盘目录（权威：
// 覆盖 replace / vendor / 自定义 GOMODCACHE 等一切场景）。
func contractsErrcodeSourceByGoList(t *testing.T) (string, bool) {
	t.Helper()
	goBin := "go"
	if rt := runtime.GOROOT(); rt != "" {
		if _, err := os.Stat(filepath.Join(rt, "bin", "go")); err == nil {
			goBin = filepath.Join(rt, "bin", "go")
		}
	}
	out, err := exec.Command(goBin, "list", "-m", "-f", "{{.Dir}}", contractsModulePath).Output()
	if err != nil {
		t.Logf("go list 定位 contracts 失败：%v", err)
		return "", false
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		t.Logf("go list 返回的模块目录为空")
		return "", false
	}
	p := filepath.Join(dir, "errcode", "errcode.go")
	if _, err := os.Stat(p); err != nil {
		t.Logf("go list 给出的路径下没有 errcode.go：%s（%v）", p, err)
		return "", false
	}
	return p, true
}

// contractsErrcodeSourceByBuildInfo 不起子进程的兜底：版本号取自
// debug.ReadBuildInfo 的依赖表，模块缓存目录取自 GOMODCACHE 或
// GOPATH/pkg/mod（go/build 的 GOPATH 解析含 $HOME/go 默认值，不需要额外起
// go env 子进程）。
func contractsErrcodeSourceByBuildInfo(t *testing.T) (string, bool) {
	t.Helper()
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		t.Logf("debug.ReadBuildInfo() 不可用")
		return "", false
	}
	version := ""
	for _, d := range bi.Deps {
		if d.Path == contractsModulePath {
			version = d.Version
			break
		}
	}
	if version == "" {
		t.Logf("debug.ReadBuildInfo() 的依赖表里没有 %s", contractsModulePath)
		return "", false
	}
	cache := os.Getenv("GOMODCACHE")
	if cache == "" {
		gopath := build.Default.GOPATH
		if gopath == "" {
			t.Logf("GOMODCACHE 与 GOPATH 均为空")
			return "", false
		}
		gopath = strings.Split(gopath, string(os.PathListSeparator))[0]
		cache = filepath.Join(gopath, "pkg", "mod")
	}
	p := filepath.Join(cache, filepath.FromSlash(contractsModulePath)+"@"+version, "errcode", "errcode.go")
	if _, err := os.Stat(p); err != nil {
		t.Logf("模块缓存里没有 contracts 源码：%s（%v）", p, err)
		return "", false
	}
	return p, true
}

// contractRegistry contracts/errcode 的登记表（从源码 AST 解析，不用正则）。
type contractRegistry struct {
	codes    map[int]string // 业务码 → 常量名
	byName   map[string]int // 常量名 → 业务码
	messages map[int]string // 业务码 → 登记文案
}

// parseContractRegistry 解析 contracts/errcode 的常量表与文案表。
//
// 用 AST 而不是正则：常量表是 const (...) 块、文案表是
// var messages = map[int]string{...} 字面量，键是常量标识符 ⇒ 正则要同时处理
// 对齐/换行/别名，很容易漏；AST 拿到的就是结构本身。
func parseContractRegistry(t *testing.T, srcPath string) contractRegistry {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, srcPath, nil, 0)
	require.NoError(t, err, "解析 contracts/errcode 源码失败：%s", srcPath)

	reg := contractRegistry{
		codes:    map[int]string{},
		byName:   map[string]int{},
		messages: map[int]string{},
	}

	// ① const 表：CodeXxx = <整数字面量>
	ast.Inspect(f, func(n ast.Node) bool {
		gd, ok := n.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			return true
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					break
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					continue
				}
				v, perr := strconv.ParseInt(lit.Value, 0, 64)
				if perr != nil {
					continue
				}
				reg.codes[int(v)] = name.Name
				reg.byName[name.Name] = int(v)
			}
		}
		return true
	})

	// ② messages 文案表：map[int]string{ CodeXxx: "..." }
	ast.Inspect(f, func(n ast.Node) bool {
		gd, ok := n.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			return true
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Values) == 0 {
				continue
			}
			cl, ok := vs.Values[0].(*ast.CompositeLit)
			if !ok {
				continue
			}
			for _, elt := range cl.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				code, ok := resolveRegistryKey(kv.Key, reg)
				if !ok {
					continue
				}
				msg, ok := kv.Value.(*ast.BasicLit)
				if !ok || msg.Kind != token.STRING {
					continue
				}
				s, perr := strconv.Unquote(msg.Value)
				if perr != nil {
					continue
				}
				reg.messages[code] = s
			}
		}
		return true
	})

	require.NotEmpty(t, reg.codes, "contracts/errcode 未解析出任何常量（登记表解析失效）")
	return reg
}

// resolveRegistryKey 把文案表的键（常量标识符或整数字面量）解析成业务码。
func resolveRegistryKey(key ast.Expr, reg contractRegistry) (int, bool) {
	switch k := key.(type) {
	case *ast.Ident:
		c, ok := reg.byName[k.Name]
		return c, ok
	case *ast.BasicLit:
		if k.Kind != token.INT {
			return 0, false
		}
		v, err := strconv.ParseInt(k.Value, 0, 64)
		if err != nil {
			return 0, false
		}
		return int(v), true
	default:
		return 0, false
	}
}

// =====================================================================
// core 源码扫描
// =====================================================================

// errCodeLiteral 一处「字面量错误码」的使用点。
type errCodeLiteral struct {
	file string // 相对模块根的路径（slash 分隔）
	line int
	code int    // 字面量码值
	ctor string // 构造器（resp.NewErrorWithMessage / pkgerrors.New …）
}

// deliveredCode 一个**经 resp / pkg-errors 出口交付**的业务码常量。
type deliveredCode struct {
	name string // 契约常量名（errcode.CodeInvalidParam）
	code int
	file string
	line int
}

// errorCtorCodeArg 判断一个调用是不是「错误码构造器」，并给出码参数的位置。
//
// 返回的第二个值是构造器展示名（用于失败输出点名）。
//
// 注意：resp.NewTypedError(c, err) 的第二参是 **error** 不是码 ⇒ 不参与扫描：
// 它的码来自 typed error 的 ErrCode() / BizCode ⇒ 由 ② 文案方向与 ③ 状态映射
// 方向覆盖。
func errorCtorCodeArg(importPath, funcName string) (int, string, bool) {
	switch importPath {
	case coreRespPkgPath:
		switch funcName {
		case "NewErrorByCode", "NewErrorWithMessage", "NewErrorWithData", "NewInternalError":
			return 1, "resp." + funcName, true
		}
	case coreErrorsPkgPath:
		switch funcName {
		case "New", "Wrap":
			return 0, "pkgerrors." + funcName, true
		}
	}
	return 0, "", false
}

// localImportPaths 建立「本地标识符 → import 路径」表。
//
// 必须按**路径**判定而不是按标识符名：errors 既可能是标准库、也可能是
// core/pkg/errors；errcode 可能被别名成 contracsErrcode（bootstrap/auto_backup.go）。
func localImportPaths(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, spec := range f.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		local := ""
		if spec.Name != nil {
			local = spec.Name.Name
		} else {
			local = path.Base(p)
		}
		if local == "_" || local == "." {
			continue
		}
		out[local] = p
	}
	return out
}

// scanCoreErrorCodes 扫全模块**非测试** .go 源文件，取出：
//   - literals：直接以 5 位字面量喂给错误构造器的码；
//   - delivered：以契约常量（errcode.CodeXxx）喂给错误构造器的码。
func scanCoreErrorCodes(t *testing.T, root string, reg contractRegistry) (literals []errCodeLiteral, delivered []deliveredCode) {
	t.Helper()
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if p != root && (strings.HasPrefix(name, ".") ||
				name == "vendor" || name == "node_modules" || name == "testdata" ||
				name == "bin" || name == "dist") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, p, nil, 0)
		if perr != nil {
			t.Errorf("解析 %s 失败：%v", p, perr)
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			rel = p
		}
		rel = filepath.ToSlash(rel)
		imports := localImportPaths(f)

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkgIdent, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			importPath, ok := imports[pkgIdent.Name]
			if !ok {
				return true
			}
			idx, ctor, ok := errorCtorCodeArg(importPath, sel.Sel.Name)
			if !ok {
				return true
			}
			if len(call.Args) <= idx {
				return true
			}
			arg := call.Args[idx]
			switch v := arg.(type) {
			case *ast.BasicLit:
				if v.Kind != token.INT {
					return true
				}
				n, perr := strconv.ParseInt(v.Value, 0, 64)
				if perr != nil {
					return true
				}
				if n < errCodeMin || n > errCodeMax {
					return true
				}
				pos := fset.Position(v.Pos())
				literals = append(literals, errCodeLiteral{
					file: rel, line: pos.Line, code: int(n), ctor: ctor,
				})
			case *ast.SelectorExpr:
				// 契约常量（errcode.CodeXxx，含别名导入）
				inner, ok := v.X.(*ast.Ident)
				if !ok {
					return true
				}
				if ip, ok := imports[inner.Name]; !ok || ip != contractsErrcodePkg {
					return true
				}
				code, ok := reg.byName[v.Sel.Name]
				if !ok {
					return true
				}
				pos := fset.Position(v.Pos())
				delivered = append(delivered, deliveredCode{
					name: v.Sel.Name, code: code, file: rel, line: pos.Line,
				})
			}
			return true
		})
		return nil
	})
	require.NoError(t, walkErr, "扫描 core 源码失败")
	return literals, delivered
}

// =====================================================================
// 方向 ①：core 源码里的字面量码不得越过契约登记清单
// =====================================================================

// TestErrcodeRegistry_CoreLiteralCodesAreRegistered 方向 ①。
//
// 漏网时输出必须点名「文件:行 + 码值」——否则拿到一条红只知道"有东西不对"，
// 不知道改哪儿。
func TestErrcodeRegistry_CoreLiteralCodesAreRegistered(t *testing.T) {
	root := coreModuleRoot(t)
	reg := parseContractRegistry(t, contractsErrcodeSource(t))
	literals, _ := scanCoreErrorCodes(t, root, reg)

	var bad []string
	for _, l := range literals {
		if _, ok := reg.codes[l.code]; ok {
			continue
		}
		bad = append(bad, fmt.Sprintf("%s:%d: 码 %d 未在 contracts/errcode 登记（构造器 %s）",
			l.file, l.line, l.code, l.ctor))
	}
	sort.Strings(bad)
	assert.Empty(t, bad,
		"core 源码里的字面量错误码越过契约登记清单（%d 处未登记）：\n%s",
		len(bad), strings.Join(bad, "\n"))
}

// =====================================================================
// 方向 ②：契约登记的每一个码都能解析出（非兜底）文案
// =====================================================================

// TestErrcodeRegistry_AllContractCodesResolveToMessage 方向 ②。
//
// errcode.Message 对**未登记**码返回兜底文案（messages[CodeUnknown]）⇒ 若某个
// 登记码没有自己的文案，调用方拿到的就是「unknown error」⇒ 用户看到一句毫无
// 信息的提示，而排查侧以为"文案已登记"。
//
// 例外：CodeUnknown 自己的登记文案**就是**兜底文案（构造如此）⇒ 它不比兜底。
func TestErrcodeRegistry_AllContractCodesResolveToMessage(t *testing.T) {
	reg := parseContractRegistry(t, contractsErrcodeSource(t))

	fallback := errcode.Message(errcode.CodeUnknown)
	require.NotEmpty(t, fallback, "errcode.Message 的兜底文案为空（errcode 包异常）")

	var bad []string
	for code, name := range reg.codes {
		want, hasMsg := reg.messages[code]
		if !hasMsg {
			bad = append(bad, fmt.Sprintf("%s(%d): contracts/errcode 的 messages 表未登记文案", name, code))
			continue
		}
		got := errcode.Message(code)
		if got != want {
			bad = append(bad, fmt.Sprintf("%s(%d): errcode.Message()=%q，登记文案=%q（多半落到了兜底文案 %q）",
				name, code, got, want, fallback))
			continue
		}
		if code == errcode.CodeUnknown {
			// 它的登记文案**就是**兜底文案（构造如此）⇒ 无法用文案区分，只能豁免。
			continue
		}
		if got == fallback {
			bad = append(bad, fmt.Sprintf("%s(%d): errcode.Message() 返回兜底文案 %q", name, code, fallback))
		}
		if strings.TrimSpace(got) == "" {
			bad = append(bad, fmt.Sprintf("%s(%d): 登记文案为空", name, code))
		}
	}
	sort.Strings(bad)
	assert.Empty(t, bad, "契约登记码解析不出文案（%d 个）：\n%s", len(bad), strings.Join(bad, "\n"))
}

// =====================================================================
// 方向 ③：契约登记码必须有 HTTP 状态映射
// =====================================================================

// knownHTTPStatusGapCodes 审计快照（2026-11-17）：这些码在契约里登记了，
// 但 core 的 httpStatusForCode 没有显式 case ⇒ 落 default ⇒ HTTP 200。
//
// 这是 core#115 / A6b 同族的遗留缺口（那批把 10016、10008、2xxxx 族逐个补进了
// switch；剩下这批没被扫到）。
//
// 为什么它不让守卫当场变红：缺口在生产代码的映射表 pkg/resp/response.go 里，
// 而守卫任务不碰生产代码 ⇒ 缺口如实登记在此，交主会话决策。最小修法：
// 在 httpStatusForCode 里补 case，例如 CodeUserDisabled → 403、
// CodeAPIKeyInvalid → 401、CodeUploadSessionGone → 404、CodeThirdParty* → 502。
//
// 本清单不是「认可这些码返回 200」：
//   - 守卫会核对「实际落 2xx 的登记码集合 ⊆ 本清单」⇒ 新增缺口（契约加码而
//     core 没映射）仍然让守卫变红；
//   - 修好一个码（映射补上）守卫仍绿 ⇒ 可以顺手把该条目从清单里删掉（删了
//     不影响判定，但清单会重新与现状对齐）。
//
// 现状影响面（实测）：这批码里只有 CodeAPIKeyInvalid 被 core 真正发射，而它的
// 发射点 pkg/middleware/apikey.go 手写 http.StatusUnauthorized ⇒ 线上行为正确；
// 本缺口是潜在陷阱（哪天有人改走 resp.NewErrorByCode ⇒ 变 200）。
var knownHTTPStatusGapCodes = map[int]string{
	errcode.CodeUnknown:           "10000 未知错误",
	errcode.CodeUploadSessionGone: "20010 上传会话不存在或已失效",
	errcode.CodeUserDisabled:      "40004 用户已禁用",
	errcode.CodeAPIKeyInvalid:     "40007 API Key 无效",
	errcode.CodeRegClosed:         "40009 注册已关闭",
	errcode.CodeEmailInvalid:      "40010 邮箱格式错误",
	errcode.CodePasswordWeak:      "40011 密码强度不足",
	errcode.CodeThirdPartySMS:     "90001 短信发送失败",
	errcode.CodeThirdPartyEmail:   "90002 邮件发送失败",
	errcode.CodeThirdPartyAPI:     "90003 第三方 API 失败",
}

// TestErrcodeRegistry_HTTPStatusMappingCoversContractCodes 方向 ③。
func TestErrcodeRegistry_HTTPStatusMappingCoversContractCodes(t *testing.T) {
	root := coreModuleRoot(t)
	reg := parseContractRegistry(t, contractsErrcodeSource(t))
	_, delivered := scanCoreErrorCodes(t, root, reg)

	// ① 每个登记码都要映射到一个可用的 HTTP 状态码。
	for code, name := range reg.codes {
		st := HTTPStatusForCode(code)
		assert.True(t, st >= 100 && st <= 599,
			"%s(%d) 的 HTTP 状态映射 %d 不是可用状态码（应落在 100..599）", name, code, st)
	}

	// ② 交付面实际用到的码 ⇒ 必须映射到非 2xx。
	//
	// 这一段是严格的（无白名单）：交付面走的就是 resp/pkg-errors 的出口，
	// 落 default ⇒ HTTP 200 ⇒ 客户端按 HTTP 状态分支会把业务错误当成功。
	deliveredSet := map[int]bool{}
	for _, d := range delivered {
		if d.code == errcode.CodeSuccess {
			continue
		}
		deliveredSet[d.code] = true
	}
	for code := range deliveredSet {
		name := reg.codes[code]
		st := HTTPStatusForCode(code)
		assert.False(t, st >= 200 && st < 300,
			"%s(%d) 经 resp/pkg-errors 出口交付，但 HTTPStatusForCode 落 default ⇒ HTTP %d（业务错误进 200）",
			name, code, st)
	}

	// ③ 契约登记、core 未显式映射 ⇒ 实际落 2xx 的码集必须 ⊆ 已知缺口清单。
	//    新增缺口 ⇒ 红；修好一个 ⇒ 仍绿。
	var gaps []string
	for code, name := range reg.codes {
		if code == errcode.CodeSuccess {
			continue
		}
		st := HTTPStatusForCode(code)
		if st < 200 || st >= 300 {
			continue
		}
		if _, known := knownHTTPStatusGapCodes[code]; known {
			continue
		}
		gaps = append(gaps, fmt.Sprintf(
			"%s(%d) ⇒ HTTP %d（core 映射表无此码；若是交付面请在 httpStatusForCode 补 case，若暂不修请登记进 knownHTTPStatusGapCodes）",
			name, code, st))
	}
	sort.Strings(gaps)
	assert.Empty(t, gaps,
		"契约登记码缺 HTTP 状态映射（落 default ⇒ HTTP 200），%d 个未登记：\n%s",
		len(gaps), strings.Join(gaps, "\n"))

	// ④ 已知缺口清单本身要合法：每个条目都得是契约登记码。
	for code, label := range knownHTTPStatusGapCodes {
		_, ok := reg.codes[code]
		assert.True(t, ok,
			"knownHTTPStatusGapCodes 里的 %s(%d) 不是契约登记码（清单过期？contracts 删码了？）", label, code)
	}
}

// =====================================================================
// 扫描器的正对照（守卫自身的失效检测）
// =====================================================================

// TestErrcodeRegistry_ScannerPositiveControl 扫描器的正对照。
//
// 守卫失效时（import 解析错、构造器改名、扫描范围被改窄），
// 「没有越界码」会假绿 ⇒ 必须有一条测试证明扫描器确实在工作：
//   - 交付码集合里必须出现这些已知的码（它们确实经 resp 出口交付）；
//   - 字面量点位数量必须达到下限（2026-11-17 实测 core 里有 29 处字面量码）。
//
// 把字面量换成契约常量是好事，会让下限变红 ⇒ 那时请同步下调下限并在提交体
// 里说明（别顺手把这条断言删了）。
func TestErrcodeRegistry_ScannerPositiveControl(t *testing.T) {
	root := coreModuleRoot(t)
	reg := parseContractRegistry(t, contractsErrcodeSource(t))
	literals, delivered := scanCoreErrorCodes(t, root, reg)

	names := map[string]bool{}
	for _, d := range delivered {
		names[d.name] = true
	}
	for _, want := range []string{
		"CodeInvalidParam", "CodeUnauthorized", "CodeNotFound", "CodeInternal",
	} {
		assert.True(t, names[want],
			"扫描器没扫到交付码 %s ⇒ import 解析 / 构造器识别 / 扫描范围已失效（守卫会假绿）", want)
	}

	assert.GreaterOrEqual(t, len(literals), 20,
		"扫描到的字面量错误码只有 %d 处（2026-11-17 实测 29 处）⇒ 扫描范围被改窄了（守卫会假绿）",
		len(literals))
}
