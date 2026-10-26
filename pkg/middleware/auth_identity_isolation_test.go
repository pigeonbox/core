package middleware

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 身份缓存隔离守卫（2026-10-26 审计）。
//
// ## 要防的回归
//
// identityCache 是**包级全局**（生产里就该是全局的），TTL 30s、按真实时钟走
// ⇒ 上一条用例写入的记录会在下一条用例里继续命中 ⇒ 断言「TTL 内命中缓存不回源」
// 这类用例只在「进程刚起、缓存是空的」时成立 ⇒ **依赖运行顺序** ⇒
// `go test -count=2` 必红（实测 calls 计数错位）⇒ CI 偶发红且红得莫名其妙。
//
// 修复方式：每条触碰身份缓存的用例先 `useFreshIdentityCache(t, …)` 换一份
// 专属缓存。⇒ 但「靠人记得加」必然复发 ⇒ 本守卫把这条变成**机械规则**：
//
//	凡在 pkg/middleware 的**测试函数**里调用了
//	identityOf / identityFresh / InvalidateIdentity / SetIdentityLoader
//	的用例，必须在同一函数体内出现 useFreshIdentityCache( ⇒ 否则红。
//
// 负对照：把某条用例的 useFreshIdentityCache(t, nil) 删掉 ⇒ 本守卫变红。

// TestIdentityCacheTestsMustBeIsolated 身份缓存用例的隔离守卫。
func TestIdentityCacheTestsMustBeIsolated(t *testing.T) {
	dir := "."
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取包目录失败: %v", err)
	}
	touchers := map[string]bool{
		"identityOf(":         true,
		"identityFresh(":      true,
		"InvalidateIdentity(": true,
		"SetIdentityLoader(":  true,
	}
	var violations []string
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("解析 %s 失败: %v", path, perr)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			body := nodeText(fset, fn.Body)
			uses := false
			for tok := range touchers {
				if strings.Contains(body, tok) {
					uses = true
					break
				}
			}
			if !uses {
				continue
			}
			checked++
			if !strings.Contains(body, "useFreshIdentityCache(") {
				violations = append(violations,
					name+":"+fn.Name.Name+" 触碰身份缓存但未调用 useFreshIdentityCache(")
			}
		}
	}
	if len(violations) > 0 {
		t.Errorf("身份缓存是包级全局 ⇒ 触碰它的用例必须用专属缓存，否则用例间互相污染（go test -count=2 必红）：\n  %s",
			strings.Join(violations, "\n  "))
	}
	if checked == 0 {
		t.Fatal("守卫本身失效：一条触碰身份缓存的用例都没扫到（解析逻辑坏了？）")
	}
}

// nodeText 取函数体的源码文本。
func nodeText(fset *token.FileSet, n ast.Node) string {
	var sb strings.Builder
	if n == nil {
		return ""
	}
	// 用位置区间从原文件取文本：FileSet 能定位到起始位置，末尾取 n.End()。
	pos := fset.Position(n.Pos())
	end := fset.Position(n.End())
	if pos.Filename == "" || end.Filename == "" || end.Offset <= pos.Offset {
		return ""
	}
	data, err := os.ReadFile(pos.Filename)
	if err != nil {
		return ""
	}
	sb.Write(data[pos.Offset:end.Offset])
	return sb.String()
}
