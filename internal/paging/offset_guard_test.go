// 分页的**收编守卫**（core#104；core#111 补正则盲区）。
//
// ⚠️ 这一组用例守四件事：
//
//	① dao/notify 层改用 paging.*Policy.Offset 后，偏移量必须与旧实现（手写乘法）
//	  逐条一致 —— 前提是调用点传入的值已按同域口径归一化。
//	② 全仓不得再出现**手写**分页偏移 —— 它没有溢出保护。
//	③ 分页解析必须走 paging.ParsePageParams（严格解析），不得回退成宽松解析。
//	④ 守卫正则必须抓得住 **任意标识符** 形态的手写偏移（core#111 盲区）。
//
// 负对照（已实跑）：
//
//	· 把 dao 层的 Offset 调用换回手写乘法 ⇒ 用例 ② 红；
//	· 把 handler 的解析换回 strconv.Atoi / convert.Str2Int ⇒ 用例 ③ 红；
//	· 往仓里塞 (req.Page - 1) * req.PageSize 形态（core#111 负对照形态）
//	  ⇒ 用例 ② 红 —— 这条形态正是旧正则 \(\s*page\s*-\s*1\s*\)\s*\* 的
//	  **盲区**（变量是 req.Page / req.PageSize，小写 page 匹配不上 ⇒ 守卫假绿），
//	  由 TestHandRolledOffsetRegexCatchesVariantForms 钉住。
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

// handRolledOffsetRe 手写分页偏移的形态：(X - 1) * Y（X 为标识符/选择器，
// Y 为标识符或数字字面量）。
//
// ⚠️ core#111：旧正则只认小写字面量 page —— 变量叫 req.Page 就漏判（假绿）。
// 现在匹配任意标识符形态：req.Page / page / p / req.Page2 等全覆盖。
// 注释行与测试文件仍跳过（测试里出现该形态是在**验证**旧行为）。
var handRolledOffsetRe = regexp.MustCompile(`\(\s*[A-Za-z_][A-Za-z0-9_.]*\s*-\s*1\s*\)\s*\*\s*[A-Za-z0-9_.]+`)

// oldHandRolledOffsetRe core#111 之前的旧正则（仅小写 page）。保留它只为
// 用例 ④ 证明「旧判据对 req.Page 形态假绿」——勿用于生产判据。
var oldHandRolledOffsetRe = regexp.MustCompile(`\(\s*page\s*-\s*1\s*\)\s*\*`)

// TestDAOPolicyOffsetIsIdempotentOnNormalizedInput 钉住「dao/notify 层用
// paging.*Policy.Offset 代替手写乘法」的**前置条件**：
// Policy.Offset 内含 Normalize ⇒ 只有当传入值已按同域口径归一化
// （page≥1、pageSize≤该域上限）时，它才是**恒等**变换。
//
// dao 域有两条口径（通用 ≤200 / 用户分享 ≤100），notify 是回落 ≤100
// （core#111 的新调用点）⇒ 都要验，且各按**自己的** Policy.Offset 断言。
//
// ⚠️ 本用例锁的是**前置条件**，不是调用点 —— 调用点由下面的守卫守。
func TestDAOPolicyOffsetIsIdempotentOnNormalizedInput(t *testing.T) {
	cases := []struct {
		name   string
		policy interface {
			Normalize(int, int) (int, int)
			Offset(int, int) int
		}
	}{
		{"dao 通用", DAOPolicy},
		{"dao 用户分享", UserSharePolicy},
		{"notify 回落", NotifyPolicy},
	}
	inputs := []struct{ page, pageSize int }{
		{1, 20}, {1, 200}, {1, 100}, {2, 20}, {7, 50}, {1000, 200}, {3, 1}, {1, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, in := range inputs {
				page, pageSize := c.policy.Normalize(in.page, in.pageSize)
				want := (page - 1) * pageSize
				got := c.policy.Offset(page, pageSize)
				if got != want {
					t.Fatalf("已归一化的 (%d,%d)：%T.Offset = %d，旧实现 = %d"+
						"（不一致 ⇒ 偏移量漂移 ⇒ 前置条件被破坏）", page, pageSize, c.policy, got, want)
				}
			}
		})
	}
}

// findHandRolledPageOffsets 扫描 root 下的生产 .go 文件，报告手写分页偏移
// 命中（rel:line: text）。跳过注释行、测试文件与 internal/paging 自身
// （口径文档里就写着这个形态）。
func findHandRolledPageOffsets(root string) ([]string, error) {
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
			if handRolledOffsetRe.MatchString(line) {
				hits = append(hits, fmt.Sprintf("%s:%d: %s", rel, i+1, trimmed))
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return hits, nil
}

// TestNoHandRolledPageOffset 全仓禁止**手写**分页偏移（core#104）。
//
// 为什么要有守卫：手写偏移**没有溢出保护** —— page 来自外部可控的 query，
// 超大页码会让 (page-1)*pageSize 溢出回绕，回绕后可能仍是**正数**
// ⇒ 只判"是否为负"的旧口径放过去 ⇒ 偏移量完全错误（查到别的页）。
func TestNoHandRolledPageOffset(t *testing.T) {
	hits, err := findHandRolledPageOffsets(repoRoot(t))
	if err != nil {
		t.Fatalf("遍历仓根失败: %v", err)
	}
	if len(hits) > 0 {
		t.Fatalf("全仓仍有**手写**分页偏移（%d 处）⇒ 没有溢出保护（超大 page 会回绕）：\n%s\n"+
			"⇒ 请改用对应域的 paging.*Policy.Offset(...)（core#104）", len(hits), strings.Join(hits, "\n"))
	}
}

// TestHandRolledOffsetRegexCatchesVariantForms 守卫正则的**判据面**（core#111）。
//
// core#111 实录：notify List 的 (req.Page - 1) * req.PageSize 从 core#104 的
// 收编里漏网，守卫照绿 —— 因为旧正则只匹配小写字面量 page，变量叫 req.Page
// 就落进盲区（断言形态写窄了 = 假绿）。
//
// 本用例钉两件事：
//   - 新正则抓得住全部变体（含 core#111 负对照形态 (req.Page - 1) * req.PageSize）；
//   - 旧正则对 req.Page 形态**假绿**（把盲区写成显式证据，防判据再写窄）。
func TestHandRolledOffsetRegexCatchesVariantForms(t *testing.T) {
	bad := []string{
		"Offset((req.Page - 1) * req.PageSize)", // core#111 负对照形态（旧正则盲区）
		"Offset((page - 1) * pageSize)",         // core#104 原形态
		"Offset((p-1)*ps)",                      // 无空格
		"Offset((q.Page - 1) * 20)",             // 乘数是字面量
		"off := (req.PageSize - 1) * pageSize",  // 非 Offset 调用同样算手写偏移
	}
	for _, line := range bad {
		if !handRolledOffsetRe.MatchString(line) {
			t.Errorf("正则应命中 %q（判据写窄了 = 假绿）", line)
		}
	}
	// 旧正则的盲区证据：req.Page 形态匹配不上（当年 notify 漏网的原因）
	if oldHandRolledOffsetRe.MatchString("Offset((req.Page - 1) * req.PageSize)") {
		t.Fatal("旧正则不应命中 req.Page 形态——若命中，盲区证据失效（请核对 oldHandRolledOffsetRe）")
	}
	// 非偏移形态不得误伤（注释行的豁免在扫描层，见变异负对照用例）
	good := []string{
		"n := (len(items) - 1)",
		"idx := (mid-1) / 2",
	}
	for _, line := range good {
		if handRolledOffsetRe.MatchString(line) {
			t.Errorf("正则不应误伤 %q", line)
		}
	}
}

// TestHandRolledOffsetGuard_MutationNegativeControl 变异负对照：往「仓」里塞
// core#111 负对照形态 (req.Page - 1) * req.PageSize ⇒ 守卫必须报红
// （证明正则+扫描链路有牙齿，不只是正则单测在自嗨）。
func TestHandRolledOffsetGuard_MutationNegativeControl(t *testing.T) {
	root := t.TempDir()
	badFile := filepath.Join(root, "fake_notify.go")
	bad := "package fake\n\nfunc off(page, size int) int { return (page - 1) * size }\n" +
		"func off2(req reqT) int { return (req.Page - 1) * req.PageSize }\n"
	if err := os.WriteFile(badFile, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	hits, err := findHandRolledPageOffsets(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 {
		t.Fatalf("变异形态应被守卫报红，实得 %d 处: %v", len(hits), hits)
	}
	// 注释行豁免（与生产扫描同款）：注释里写该形态不算手写偏移
	if err := os.WriteFile(filepath.Join(root, "commented.go"), []byte("package fake\n\n// (req.Page - 1) * req.PageSize 仅是注释\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 干净文件 ⇒ 不报（判据不是恒红）
	if err := os.WriteFile(filepath.Join(root, "clean.go"), []byte("package fake\n\nfunc ok() int { return 0 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(badFile); err != nil {
		t.Fatal(err)
	}
	if hits, err = findHandRolledPageOffsets(root); err != nil {
		t.Fatal(err)
	} else if len(hits) != 0 {
		t.Fatalf("注释行/干净文件不应报红: %v", hits)
	}
	hits, err = findHandRolledPageOffsets(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("干净仓不应报红: %v", hits)
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
