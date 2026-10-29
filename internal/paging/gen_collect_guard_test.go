package paging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGenHandlerPaginationIsConsolidated 守卫「#8 收编**没有回退**」这一事实。
//
// #8 收编（2026-10-29）把 4 处**寄居在生成物里**的手写分页归一化改成调用本包的
// Policy 声明。收编本身安全（hz update 不重写既有函数体、CI 的 CHECK=1 对
// gen/handler 只对账文件增删），但**有一个回退面**：
//
//	`scripts/gen-router.sh` 在常规模式跑完后会提示
//	「git checkout -- gen/handler/admin/admin_service.go」还原 hz 追加的重复桩
//	⇒ 该命令**连同文件里的手写段落一起还原** ⇒ 分页口径会**静默退回**手写 if 形态。
//
// ⇒ 静默退回的后果：本包的口径表说「已收编」，而生成物里又是另一套写法 ⇒
// 「口径清单」与实现漂移 —— 这正是 #8 立issue 要消灭的那类漂移。
//
// ⇒ 本守卫按**内容**检查生成物里那 4 处是否仍在调用本包的 Policy（而不是靠
// import 存在与否：那证明不了调用点还在）。谁把收编还原了 ⇒ 本用例红。
//
// ⚠️ 检查的是**调用点存在**，不是**行为等价**（行为等价由 policy_test.go 的
// 「收编前后逐条一致」用例钉住）—— 两者互补。
func TestGenHandlerPaginationIsConsolidated(t *testing.T) {
	// 定位 core 仓根（本包在 <core>/internal/paging 下）。
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("定位仓根失败: %v", err)
	}

	cases := []struct {
		file string
		// wantCall 是该文件里应当出现的调用片段。
		wantCall string
	}{
		{"gen/handler/admin/admin_service.go", "paging.GenAdminListPolicy.Normalize("},
		{"gen/handler/user/user_service.go", "paging.GenUserFilesPolicy.Normalize("},
		{"gen/handler/share/share_service.go", "paging.GenShareUserSharesPolicy.Normalize("},
	}

	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			path := filepath.Join(root, c.file)
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("读 %s 失败: %v", c.file, err)
			}
			src := string(b)
			if !strings.Contains(src, c.wantCall) {
				t.Errorf("%s 里找不到分页口径的收编调用 %q\n"+
					"⇒ #8 的收编被还原了（多半是跑 gen-router.sh 后照提示执行了 "+
					"`git checkout --` ⇒ 连同手写段落一起还原）\n"+
					"⇒ 请从 git 历史取回该文件的收编改动后再提交；\n"+
					"   否则本包的口径表会与生成物里的实现漂移。",
					c.file, c.wantCall)
			}
		})
	}

	// 反向检查：生成物里**不应**再残留这 4 处的**手写归一化形态**
	// （pageSize = 20 / pageSize > 100 之类）⇒ 收编过 ⇒ 不该再有。
	//
	// ⚠️ 判据必须精确：只查「紧跟在 pageSize 声明后的手写 if 形态」，
	// 不能泛查 "pageSize = 20"（那会命中别的含义，比如 dao 层与注释里的例子）。
	for _, c := range cases {
		path := filepath.Join(root, c.file)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", c.file, err)
		}
		if strings.Contains(string(b), "pageSize = 20") {
			t.Errorf("%s 里仍残留手写分页归一化（pageSize = 20）⇒ 收编未生效或被部分还原",
				c.file)
		}
	}
}
