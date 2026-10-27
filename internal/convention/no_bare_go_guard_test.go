// no_bare_go_guard_test.go 规则 1：禁裸 `go` 语句（机械守卫）。
//
// ## 为什么立这条
//
// 裸 goroutine 的 panic **无人接** ⇒ 直接带走整个进程。core 是长驻服务
// （HTTP 面 + 后台任务 + 联邦心跳）⇒ 一个后台协程里的 nil 解引用就能把整站
// 打挂，且现场只剩一句 stack——用户看到的是"服务不可用"。
//
// 修法不是"记得 recover"，而是**用 oekit/concurrency/async 的 GoSafe 家族**：
// panic 被收编，并经**全局 handler** 上报（bootstrap.go 已装
// async.SetPanicHandler ⇒ 落结构化日志 + stack，不是静默吞掉）。
//
// ## 例外：主服务协程
//
// 白名单只登记「主服务协程」——进程的存在意义就是它，panic 应**带崩进程**
// 让 supervisor（容器 restart policy）重启它：改成 GoSafe 会让错误永不投递到
// 主流程 ⇒ 主流程挂死 ⇒ 节点"活着但不服务"，比直接死掉更难诊断。
//
// ## 棘轮
//
// 白名单按「文件 + 所在函数」计数，**只许减不许增**；条目失效（该处已无裸 go）
// 也判红 ⇒ 白名单不会悄悄变成"什么都放行"的垃圾桶。
package convention

import (
	"fmt"
	"go/ast"
	"sort"
	"strings"
	"testing"
)

// bareGoAllow 白名单条目。
type bareGoAllowEntry struct {
	file   string // 模块根相对路径（斜杠分隔）
	fn     string // 所在函数名
	count  int    // 允许的裸 go 语句条数
	reason string // 放行理由
}

// bareGoAllow 例外白名单（core 当前为空）。
//
// 新增条目须写明「为什么 panic 在这里应当带崩进程」，并接受棘轮（只许减）。
var bareGoAllow = []bareGoAllowEntry{}

// TestNoBareGoStatement 规则 1：生产代码里不得出现裸 `go` 语句。
//
// 判据是 **AST 级**（`go/ast` 的 *ast.GoStmt）：文本 grep 会把注释、字符串、
// 以及 `//go:xxx` 指令误判，也认不出 `go someFunc()` 这种非字面量形态。
func TestNoBareGoStatement(t *testing.T) {
	root := moduleRoot(t)

	// 实际命中：file → func → 条数
	actual := map[string]map[string]int{}
	var violations []string

	walkProductionGoFiles(t, root, func(rel, abs string) {
		fset, f := parseGoFile(t, abs)
		ast.Inspect(f, func(n ast.Node) bool {
			gs, ok := n.(*ast.GoStmt)
			if !ok {
				return true
			}
			fn := enclosingFuncName(f, gs)
			if actual[rel] == nil {
				actual[rel] = map[string]int{}
			}
			actual[rel][fn]++
			line := fset.Position(gs.Pos()).Line
			allowed := false
			for _, e := range bareGoAllow {
				if e.file == rel && e.fn == fn && actual[rel][fn] <= e.count {
					allowed = true
					break
				}
			}
			if !allowed {
				violations = append(violations, fmt.Sprintf(
					"规则1 裸 go 语句: %s:%d（函数 %s）——改用 async.GoSafe/GoSafeLabeled"+
						"（panic 经全局 async.SetPanicHandler 落日志，不静默）", rel, line, fn))
			}
			return true
		})
	})

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Errorf("发现 %d 处裸 go 语句:\n%s", len(violations), strings.Join(violations, "\n"))
	}

	// 棘轮二：白名单条目失效（该处已无裸 go / 条数减少）⇒ 判红，请删条目。
	for _, e := range bareGoAllow {
		got := actual[e.file][e.fn]
		if got < e.count {
			t.Errorf("规则1 白名单条目失效: %s 的 %s 登记 %d 处、实际 %d 处 —— 请删除或下调条目（棘轮只许减）",
				e.file, e.fn, e.count, got)
		}
	}
}
