// error_envelope_guard_test.go 规则 2：错误信封方言机械守卫（core#114 A6b）。
//
// ## 为什么立这条
//
// core#114 的病根是「错误出口方言分裂」：同一仓里混着 legacy map（无 trace_id、
// 状态硬编码）、裸 err.Error() 文案出网（SQL/驱动文本泄漏）与全站信封三种写法。
// A6/A6b 已把 gen/handler 各面收编到 resp.NewErrorWithMessage / NewInternalError，
// 本守卫让残留**在结构上无法复发**：
//
//	规则 A：目标文件里不得再出现 legacy 错误 map（map 字面量同时含 code+message
//	        键且 code 不是 0 字面量）——成功面的 legacy 形状（code: 0）是既有
//	        wire 契约，不在此列；
//	规则 B：不得把 err.Error() 拼接进响应文案（"xx失败: "+err.Error() 形态）——
//	        内部细节一律走 NewInternalError（进日志、对外脱敏）。
//
// 负对照（已实跑）：把 gen/handler/chunk 任一错误出口改回 legacy map ⇒ 规则 A 转红；
// 把 NewInternalError 调用改回 "xx失败: "+err.Error() ⇒ 规则 B 转红。
package convention

import (
	"fmt"
	"go/ast"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// envelopeSweepFiles A6b 错误信封收编覆盖的 gen/handler 文件。
var envelopeSweepFiles = map[string]bool{
	"gen/handler/chunk/chunk_service.go":             true,
	"gen/handler/maintenance/maintenance_service.go": true,
	"gen/handler/user/user_service.go":               true,
	"gen/handler/user/user_trust.go":                 true,
	"gen/handler/p2p/p2_ppublic_service.go":          true,
	"gen/handler/p2p/admin_p2_pservice.go":           true,
	"gen/handler/presign/presign_service.go":         true,
	"gen/handler/admin/admin_manage.go":              true,
}

func TestErrorEnvelopeSweepNoLegacyDialect(t *testing.T) {
	root := moduleRoot(t)
	seen := map[string]bool{}
	var violations []string

	walkProductionGoFiles(t, root, func(rel, abs string) {
		if !envelopeSweepFiles[rel] {
			return
		}
		seen[rel] = true
		fset, f := parseGoFile(t, abs)
		ast.Inspect(f, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CompositeLit:
				if _, ok := v.Type.(*ast.MapType); !ok {
					return true
				}
				codeVal, hasCode := mapLitEntry(v, "code")
				_, hasMsg := mapLitEntry(v, "message")
				if hasCode && hasMsg && !isZeroLit(codeVal) {
					violations = append(violations, fmt.Sprintf(
						"规则A legacy 错误 map: %s:%d —— 错误出口统一走 resp.NewErrorWithMessage/NewInternalError（trace_id + 状态映射）",
						rel, fset.Position(v.Pos()).Line))
				}
			case *ast.BinaryExpr:
				if v.Op == token.ADD && (callsErrorMethod(v.X) || callsErrorMethod(v.Y)) {
					violations = append(violations, fmt.Sprintf(
						"规则B err.Error() 拼接进文案: %s:%d —— 内部错误走 resp.NewInternalError（脱敏 + 进日志）",
						rel, fset.Position(v.Pos()).Line))
				}
			}
			return true
		})
	})

	// 清单闭合：登记的文件必须真实存在（文件被改名/删除时守卫不得静默放行）
	for rel := range envelopeSweepFiles {
		if !seen[rel] {
			violations = append(violations, fmt.Sprintf("登记文件缺失: %s —— 更新 envelopeSweepFiles 清单", rel))
		}
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Errorf("发现 %d 处错误信封方言残留:\n%s", len(violations), strings.Join(violations, "\n"))
	}
}

// mapLitEntry 取 map 字面量里指定字符串键的值表达式。
func mapLitEntry(cl *ast.CompositeLit, key string) (ast.Expr, bool) {
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		lit, ok := kv.Key.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}
		if s, err := strconv.Unquote(lit.Value); err == nil && s == key {
			return kv.Value, true
		}
	}
	return nil, false
}

// isZeroLit 表达式是否为整数字面量 0（成功面 legacy 形状的 code 值）。
func isZeroLit(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.INT && lit.Value == "0"
}

// callsErrorMethod 表达式里是否调用了零参 Error() 方法（err.Error() 形态）。
func callsErrorMethod(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Error" && len(call.Args) == 0 {
			found = true
		}
		return true
	})
	return found
}
