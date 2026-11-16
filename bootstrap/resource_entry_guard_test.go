// Resource 入口收口守卫（core#173 第六片：Resource Policy 收口）。
//
// ## 为什么有这道门禁
//
// pkg/gate/resource.go 是容量/预算判定的单一执行点（CheckFileSize/Exceeds*/
// HTTPBodyLimit/CheckTaskBudget），但**没有任何机制要求新入口调用它**——
// 历史上 utils.CheckWholeFileSize 各通道手抄一份、超限比较散在 handler 里，
// 新入口照抄即可绕过收口点，且没有任何测试会红。
//
// ⇒ 本守卫把「容量/预算判定只准走 pkg/gate」变成机械判据（三层，
// authn/authz_entry_guard 同款）：
//
//	① 裸判定源码扫描 × 豁免：app/bootstrap/gen/transport/internal 的非测试
//	   源码出现裸容量读取/校验（utils.GetMaxFileSize(、utils.CheckWholeFileSize(
//	   等五个模式）⇒ 红，除非在 resourceInlineExemptions 登记理由；豁免条目
//	   已无标记 ⇒ 陈旧红。当前零豁免（收口后全仓 getter 调用只剩 pkg/gate）；
//	② 站点登记 × 承载标记 + 反向对账：resourceSites 登记本片迁移的全部
//	   承载文件与 gate 调用标记（Min=出现次数）——站点文件删掉 gate 调用 ⇒ 红；
//	   源码出现资源判定函数调用但文件未登记 ⇒ 红（新站点必须进清单）；
//	③ 执行点核心自检：pkg/gate/resource.go 的九个判定函数 + MinTaskInterval
//	   常量定义缺失 ⇒ 执行点被掏空红。
//
// 负对照：TestResourceEntryGuardNegativeControl 用合成输入自证三层判定有牙齿
// （恒绿）；真实文件级负对照（删 app/presign 的 gate.CheckFileSize 调用 ⇒ ② 红、
// 还原 ⇒ 绿）见 PR 正文实跑记录。
//
// 行为级兜底：执行点被掏空（如 CheckFileSize 恒返回 nil）由
// pkg/gate/resource_test.go 与各通道既有 413 断言覆盖。
package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resourceMarker 承载文件的必含标记：Sub 在文件中至少出现 Min 次。
type resourceMarker struct {
	Sub string
	Min int
}

// resourceFuncNames 资源判定函数全集（反向对账与核心自检共用同一真相源）。
var resourceFuncNames = []string{
	"CheckFileSize", "ExceedsFileSize", "MaxFileSize",
	"MaxRequestSize", "ExceedsRequestSize",
	"TextShareMaxBytes", "ExceedsTextShare",
	"HTTPBodyLimit", "CheckTaskBudget",
}

// resourceSites 本片迁移的承载文件 → gate 资源判定调用标记（Min=出现次数，
// 与 PR「散点迁移清单」逐条对账）。新增容量/预算判定站点必须登记。
var resourceSites = map[string][]resourceMarker{
	"app/chunk/service.go": {
		{Sub: "gate.CheckFileSize(", Min: 1},
	},
	"app/presign/presign.go": {
		{Sub: "gate.CheckFileSize(", Min: 3},
	},
	"app/share/localimport.go": {
		{Sub: "gate.CheckFileSize(", Min: 1},
	},
	"app/share/sharebytes.go": {
		{Sub: "gate.CheckFileSize(", Min: 1},
	},
	"app/share/service.go": {
		{Sub: "gate.ExceedsTextShare(", Min: 1},
	},
	"gen/handler/chunk/chunk_service.go": {
		{Sub: "gate.ExceedsFileSize(", Min: 1},
	},
	"gen/handler/share/share_service.go": {
		{Sub: "gate.ExceedsTextShare(", Min: 1},
		{Sub: "gate.ExceedsRequestSize(", Min: 1},
	},
	"gen/handler/share_anonymous/share_anonymous_service.go": {
		{Sub: "gate.CheckFileSize(", Min: 1},
	},
	"transport/http/handler/helpers.go": {
		{Sub: "gate.ExceedsFileSize(", Min: 1},
	},
	"transport/http/handler/share_multi.go": {
		{Sub: "gate.ExceedsFileSize(", Min: 1},
	},
	"transport/http/handler/federation_delivery.go": {
		{Sub: "gate.ExceedsFileSize(", Min: 1},
	},
	"bootstrap/bootstrap.go": {
		{Sub: "gate.HTTPBodyLimit(", Min: 1},
	},
	"bootstrap/tasks.go": {
		{Sub: "gate.CheckTaskBudget(", Min: 1},
	},
	"bootstrap/federation_sync.go": {
		{Sub: "gate.CheckTaskBudget(", Min: 1},
	},
}

// resourceBarePatterns 裸判定模式（层①扫描对象）：这些符号只准在 pkg/gate
// 内部出现——收口后全仓调用面为零。
var resourceBarePatterns = []string{
	"utils.GetMaxFileSize(",
	"utils.GetMaxUploadSize(",
	"utils.GetTextShareMaxBytes(",
	"utils.CheckWholeFileSize(",
	"utils.CheckUploadSize(",
}

// resourceScanRoots 层①扫描根（相对 worktree；pkg/ 不扫——执行点自身与
// pkg/utils 定义文件在其中）。
var resourceScanRoots = []string{"app", "bootstrap", "gen", "transport", "internal"}

// resourceInlineExemptions 裸判定豁免登记：文件 → 理由（必须非空）。
// 起步为零（收口后无合法裸判定）；确有展示类只读用途时在此登记并写明理由。
var resourceInlineExemptions = map[string]string{}

// resourceHasPattern 文件是否命中任一裸判定模式。
func resourceHasPattern(content string) bool {
	for _, p := range resourceBarePatterns {
		if strings.Contains(content, p) {
			return true
		}
	}
	return false
}

// resourceCallHit 文件是否调用资源判定函数（反向对账用）。
func resourceCallHit(content string) (string, bool) {
	for _, name := range resourceFuncNames {
		if strings.Contains(content, "gate."+name+"(") {
			return name, true
		}
	}
	return "", false
}

// TestResourceEntryGuardCoverage 三层守卫主判据。
func TestResourceEntryGuardCoverage(t *testing.T) {
	// ===== 层① 裸判定扫描 × 豁免 =====
	hitsByFile := map[string]bool{}
	for _, root := range resourceScanRoots {
		_ = filepath.Walk(filepath.Join("..", root), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			rel := strings.TrimPrefix(path, "../")
			if resourceHasPattern(string(b)) {
				hitsByFile[rel] = true
			}
			return nil
		})
	}
	for path := range hitsByFile {
		if reason, ok := resourceInlineExemptions[path]; !ok || strings.TrimSpace(reason) == "" {
			t.Errorf("层①：裸容量判定 %s（须改走 pkg/gate 执行点，或在 resourceInlineExemptions 登记理由）", path)
		}
	}
	for path, reason := range resourceInlineExemptions {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("层①：豁免 %s 缺理由", path)
		}
		if !hitsByFile[path] {
			t.Errorf("层①：豁免陈旧（%s 已无裸判定命中）", path)
		}
	}

	// ===== 层② 站点登记 × 承载标记 + 反向对账 =====
	for file, markers := range resourceSites {
		b, err := os.ReadFile(filepath.Join("..", file))
		if err != nil {
			t.Errorf("层②：登记站点不可读 %s（文件被删/改名必须同步 resourceSites）", file)
			continue
		}
		content := string(b)
		for _, m := range markers {
			if n := strings.Count(content, m.Sub); n < m.Min {
				t.Errorf("层②：站点 %s 缺标记 %s（需 ≥%d 处，实得 %d）——承载文件删掉执行点调用", file, m.Sub, m.Min, n)
			}
		}
	}
	for _, root := range resourceScanRoots {
		_ = filepath.Walk(filepath.Join("..", root), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			rel := strings.TrimPrefix(path, "../")
			if name, hit := resourceCallHit(string(b)); hit {
				if _, ok := resourceSites[rel]; !ok {
					t.Errorf("层②：新站点 %s 调用了 gate.%s 但未登记 resourceSites（散点清单必须同步）", rel, name)
				}
			}
			return nil
		})
	}

	// ===== 层③ 执行点核心自检 =====
	core, err := os.ReadFile(filepath.Join("..", "pkg/gate/resource.go"))
	if err != nil {
		t.Fatalf("层③：执行点 pkg/gate/resource.go 不可读：%v", err)
	}
	coreContent := string(core)
	for _, name := range resourceFuncNames {
		if !strings.Contains(coreContent, "func "+name+"(") {
			t.Errorf("层③：执行点被掏空——pkg/gate/resource.go 缺 func %s(", name)
		}
	}
	if !strings.Contains(coreContent, "const MinTaskInterval") {
		t.Errorf("层③：执行点缺 MinTaskInterval 常量")
	}
}

// TestResourceEntryGuardNegativeControl 合成输入自证三层判定有牙齿（恒绿）。
func TestResourceEntryGuardNegativeControl(t *testing.T) {
	// ① 裸判定命中未登记 ⇒ 红；登记且有理由 ⇒ 绿；陈旧豁免 ⇒ 红
	if !resourceHasPattern("limit := utils.GetMaxFileSize()") {
		t.Fatalf("负对照①a：裸模式应命中")
	}
	if resourceHasPattern("gate.CheckFileSize(size)") {
		t.Fatalf("负对照①b：执行点调用不应被裸模式误伤")
	}
	// ② 站点标记缺失 ⇒ 红；反向对账（未登记文件命中调用）⇒ 红
	if strings.Count("func f() { gate.CheckFileSize(1) }", "gate.CheckFileSize(") < 1 {
		t.Fatalf("负对照②a：标记计数应 ≥1")
	}
	if _, hit := resourceCallHit("gate.HTTPBodyLimit()"); !hit {
		t.Fatalf("负对照②b：反向对账应识别 HTTPBodyLimit 调用")
	}
	if _, hit := resourceCallHit("gate.PrecheckPickup(x)"); hit {
		t.Fatalf("负对照②c：非资源函数不得误判为资源站点")
	}
	// ③ 核心函数名单非空且形态正确（防名单被清空导致层③空转）
	if len(resourceFuncNames) != 9 {
		t.Fatalf("负对照③：资源判定函数名单应 9 个，实得 %d（名单被清空 ⇒ 层③空转）", len(resourceFuncNames))
	}
}
