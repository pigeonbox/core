// 分页的**收编守卫**（core#104）。
//
// ⚠️ 这一组用例守三件事：
//
//	① dao 层改用 paging.DAOPolicy.Offset 后，偏移量必须与旧实现（手写乘法）
//	  逐条一致 —— 前提是调用点传入的值已按 dao 域口径归一化。
//	② 全仓不得再出现**手写**分页偏移 —— 它没有溢出保护。
//	③ 分页解析必须走 paging.ParsePageParams（严格解析），不得回退成宽松解析。
//
// 负对照：
//
//	· 把 dao 层的 Offset 调用换回手写乘法 ⇒ 用例 ② 红；
//	· 把 handler 的解析换回 strconv.Atoi / convert.Str2Int ⇒ 用例 ③ 红。
package paging

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestDAOPolicyOffsetIsIdempotentOnNormalizedInput 钉住「dao 层用
// paging.DAOPolicy.Offset 代替手写乘法」的**前置条件**：
// Policy.Offset 内含 Normalize ⇒ 只有当传入值已按 dao 域口径归一化
// （page≥1、pageSize≤200）时，它才是**恒等**变换。
//
// dao 域有两条口径（通用 ≤200 / 用户分享 ≤100）⇒ 两个都要验。
//
// ⚠️ 本用例锁的是**前置条件**，不是调用点 —— 调用点由下面的守卫守。
func TestDAOPolicyOffsetIsIdempotentOnNormalizedInput(t *testing.T) {
	cases := []struct {
		name   string
		policy interface{ Normalize(int, int) (int, int) }
	}{
		{"dao 通用", DAOPolicy},
		{"dao 用户分享", UserSharePolicy},
	}
	inputs := []struct{ page, pageSize int }{
		{1, 20}, {1, 200}, {1, 100}, {2, 20}, {7, 50}, {1000, 200}, {3, 1}, {1, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, in := range inputs {
				page, pageSize := c.policy.Normalize(in.page, in.pageSize)
				want := (page - 1) * pageSize
				got := DAOPolicy.Offset(page, pageSize)
				if got != want {
					t.Fatalf("已归一化的 (%d,%d)：DAOPolicy.Offset = %d，旧实现 = %d"+
						"（不一致 ⇒ 偏移量漂移 ⇒ 前置条件被破坏）", page, pageSize, got, want)
				}
			}
		})
	}
}

// TestNoHandRolledPageOffset 全仓禁止**手写**分页偏移（core#104）。
//
// 为什么要有守卫：手写偏移**没有溢出保护** —— page 来自外部可控的 query，
// 超大页码会让 (page-1)*pageSize 溢出回绕，回绕后可能仍是**正数**
// ⇒ 只判"是否为负"的旧口径放过去 ⇒ 偏移量完全错误（查到别的页）。
//
// ⚠️ 判据精度：只查 `(page-1)*` 这一形态，且跳过注释行与测试文件
// （测试里出现该形态是**在验证**旧行为，不是生产代码）。
func TestNoHandRolledPageOffset(t *testing.T) {
	root := repoRoot(t)
	re := regexp.MustCompile(`\(\s*page\s*-\s*1\s*\)\s*\*`)

	skipDirs := map[string]bool{
		".git": true, "node_modules": true, "vendor": true, "testdata": true,
		"dist": true, "build": true,
	}

	var hits []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			rel, _ := filepath.Rel(root, path)
			if rel == filepath.Join("internal", "paging") {
				// 本包自身（口径文档里就写着这个形态）⇒ 跳过。
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if re.MatchString(line) {
				hits = append(hits, fmt.Sprintf("%s:%d: %s", rel, i+1, trimmed))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历仓根失败: %v", err)
	}
	if len(hits) > 0 {
		t.Fatalf("全仓仍有**手写**分页偏移（%d 处）⇒ 没有溢出保护（超大 page 会回绕）：\n%s\n"+
			"⇒ 请改用对应域的 paging.*Policy.Offset(...)（core#104）", len(hits), strings.Join(hits, "\n"))
	}
}

// TestPaginationParsingIsConsolidated 守卫「分页解析收编没被回退」。
//
// 收编前，这 4 处各自宽松解析（strconv.Atoi 忽略错误 / convert.Str2Int 失败取默认）
// ⇒ 非数字页码静默变成默认页 ⇒ 调用方分不清"翻到了第 abc 页"和"只给了第 1 页"。
func TestPaginationParsingIsConsolidated(t *testing.T) {
	root := repoRoot(t)
	files := []string{
		"gen/handler/admin/admin_manage.go",
		"gen/handler/share/share_service.go",
		"gen/handler/notify/notify_service.go",
		"app/admin/files_filter.go",
	}
	// 旧的宽松解析形态（收编后不得残留）。
	forbidden := []string{
		`convert.Str2Int(string(c.Query("page")`,
		`convert.Str2Int(string(c.Query("page_size")`,
		`strconv.Atoi(c.DefaultQuery("page"`,
		`strconv.Atoi(c.DefaultQuery("page_size"`,
		`strconv.Atoi(get("page")`,
		`strconv.Atoi(get("page_size")`,
	}
	for _, f := range files {
		t.Run(f, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
			if err != nil {
				t.Fatalf("读 %s 失败: %v", f, err)
			}
			src := string(b)
			if !strings.Contains(src, "paging.ParsePageParams(") {
				t.Errorf("%s 里找不到 paging.ParsePageParams( ⇒ 分页解析收编被回退", f)
			}
			for _, bad := range forbidden {
				if strings.Contains(src, bad) {
					t.Errorf("%s 里仍残留宽松分页解析 %q ⇒ 非数字会被静默当默认页（core#104 要治的病）", f, bad)
				}
			}
		})
	}
}

// repoRoot 定位 core 仓根（本包在 <core>/internal/paging 下）。
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("定位仓根失败: %v", err)
	}
	return root
}
