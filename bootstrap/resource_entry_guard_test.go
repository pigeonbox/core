// Resource 入口收口守卫（core#173 第六片：Resource Policy 收口）。
//
// ## 为什么有这道门禁
//
// pkg/gate/resource.go 是容量/预算判定的单一执行点（CheckFileSize/Exceeds*/
// HTTPBodyLimit/CheckTaskBudget），但**没有任何机制要求新入口调用它**——
// 历史上 utils.CheckWholeFileSize 各通道手抄一份、超限比较散在 11 个文件里，
// 新入口照抄即可绕过收口点，且没有任何测试会红（2026-10-19 / 2026-10-30 两次
// 事故都是「判定散落、无人对账」的直接后果）。
//
// ⇒ 本守卫把「容量/预算判定只准走 pkg/gate」变成机械判据（四查，authn/authz/
// pickup 守卫同款）：
//
//	① 矩阵 upload 桶 × 口径登记：router_policy_matrix.txt（#203 产物，只读）里
//	   频控桶=upload 的每条路由必须在 resourceUploadSurfaces 登记 Resource 口径
//	   （判定站点 key 或显式 "N/A：理由"）——新上传面未登记、口径指向幽灵站点、
//	   裸 N/A 无理由、登记陈旧 ⇒ 红；
//	② 裸判定源码扫描 × 豁免：非测试源码出现裸容量读取/校验（utils.GetMaxFileSize(
//	   等五个模式）或 config.Upload.{MaxFileSize,UploadSize,TextMaxBytes} 的裸比较
//	   ⇒ 红，除非在 resourceInlineExemptions 登记理由；豁免条目已无标记 ⇒ 陈旧红。
//	   当前**零豁免**（收口后全仓 getter 调用只剩 pkg/gate 执行点）；
//	③ 站点登记 × 承载标记 + 反向对账：resourceSites 登记本片迁移的全部承载文件
//	   与 gate 调用标记（Min=出现次数）——站点文件删掉 gate 调用 ⇒ 红；源码出现
//	   资源/配额判定函数调用但文件未登记 ⇒ 红（新站点必须进清单）；
//	④ 执行点核心自检 + 后台循环预算口：pkg/gate/resource.go 的十一个判定函数 +
//	   MinTaskInterval 常量缺失 ⇒ 执行点被掏空红；bootstrap 非测试源码出现裸
//	   loop.Run（绕过 startBGLoop 预算口）⇒ 红。
//
// 负对照：TestResourceEntryGuardNegativeControl 用合成输入自证四查有牙齿
// （恒绿）；真实文件级负对照（删 app/presign 的 gate.CheckFileSize 调用 ⇒ ③ 红、
// 还原 ⇒ 绿）见 PR 正文实跑记录。
//
// 行为级兜底：执行点被掏空（如 CheckFileSize 恒返回 nil）由
// pkg/gate/resource_test.go 与各通道既有 413 断言覆盖。
package bootstrap

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// resourceMarker 承载文件的必含标记：Sub 在文件中至少出现 Min 次。
type resourceMarker struct {
	Sub string
	Min int
}

// resourceFuncNames 资源判定函数全集（反向对账与核心自检共用同一真相源）。
// 前九个是本片新增的 Resource 面；CheckUploadSizeCap（#196 前已收口的总量闸）
// 与 CheckAnonymousQuota（2026-10-03 收口的匿名日配额）一并纳入反向对账——
// 「所有容量判定都过统一入口」包含配额两闸。
var resourceFuncNames = []string{
	"CheckFileSize", "ExceedsFileSize", "MaxFileSize",
	"MaxRequestSize", "ExceedsRequestSize",
	"TextShareMaxBytes", "ExceedsTextShare",
	"HTTPBodyLimit", "CheckTaskBudget",
	"CheckUploadSizeCap", "CheckAnonymousQuota",
	"BatchCapBelowFileCap",
}

// resourceSites 本片迁移的承载文件 → gate 资源/配额判定调用标记（Min=出现次数，
// 与 PR「散点迁移清单」逐条对账）。新增容量/预算判定站点必须登记。
var resourceSites = map[string][]resourceMarker{
	"app/chunk/service.go": {
		{Sub: "gate.CheckFileSize(", Min: 1},
		{Sub: "gate.CheckUploadSizeCap(", Min: 1},
		{Sub: "func ValidateChunkBytes(", Min: 1},
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
		{Sub: "gate.CheckUploadSizeCap(", Min: 1},
	},
	"app/share/quota_reserve.go": {
		{Sub: "gate.CheckUploadSizeCap(", Min: 1},
		{Sub: "reserveUploadCaps(", Min: 1},
	},
	"gen/handler/chunk/chunk_service.go": {
		{Sub: "gate.ExceedsFileSize(", Min: 1},
		{Sub: "gate.CheckAnonymousQuota(", Min: 1},
		{Sub: "chunkService.ValidateChunkBytes(", Min: 1},
	},
	"gen/handler/share/share_service.go": {
		{Sub: "gate.ExceedsTextShare(", Min: 1},
		{Sub: "gate.ExceedsRequestSize(", Min: 1},
		{Sub: "gate.CheckAnonymousQuota(", Min: 1},
	},
	"gen/handler/share_anonymous/share_anonymous_service.go": {
		{Sub: "gate.CheckFileSize(", Min: 1},
		{Sub: "gate.CheckAnonymousQuota(", Min: 1},
	},
	"gen/handler/presign/presign_service.go": {
		{Sub: "gate.CheckAnonymousQuota(", Min: 1},
	},
	"transport/http/handler/helpers.go": {
		{Sub: "gate.ExceedsFileSize(", Min: 1},
	},
	"transport/http/handler/share_multi.go": {
		{Sub: "gate.ExceedsFileSize(", Min: 1},
		{Sub: "gate.CheckAnonymousQuota(", Min: 1},
	},
	"transport/http/handler/file_request.go": {
		{Sub: "gate.CheckAnonymousQuota(", Min: 1},
	},
	"transport/http/handler/federation_delivery.go": {
		{Sub: "gate.ExceedsFileSize(", Min: 1},
		{Sub: "gate.CheckAnonymousQuota(", Min: 1},
	},
	"app/mcp/server.go": {
		{Sub: "mcpBodyLimit(", Min: 1},
	},
	"app/config/policy.go": {
		{Sub: "gate.BatchCapBelowFileCap(", Min: 1},
	},
	"bootstrap/handlers_public.go": {
		{Sub: "gate.TextShareMaxBytes(", Min: 1},
	},
	"bootstrap/bootstrap.go": {
		{Sub: "gate.HTTPBodyLimit(", Min: 1},
	},
	"bootstrap/tasks.go": {
		{Sub: "func startBGLoop(", Min: 1},
		{Sub: "gate.CheckTaskBudget(", Min: 1},
	},
	"bootstrap/federation_sync.go": {
		{Sub: "gate.CheckTaskBudget(", Min: 1},
	},
}

// resourceBarePatterns 裸判定模式（层②扫描对象）：这些符号只准在 pkg/gate
// 执行点与 pkg/utils 访问器定义处出现——收口后全仓调用面为零。
var resourceBarePatterns = []string{
	"utils.GetMaxFileSize(",
	"utils.GetMaxUploadSize(",
	"utils.GetTextShareMaxBytes(",
	"utils.CheckWholeFileSize(",
	"utils.CheckUploadSize(",
}

// resourceBareRe 裸判定正则（层②）：config.Upload 三个容量旋钮的**裸比较**。
// 只拦比较形态——配置回显（/api/config 下发）、值域校验（< 0）不算判定。
var resourceBareRe = []struct {
	Name string
	Re   *regexp.Regexp
}{
	{"config.Upload.MaxFileSize 裸比较", regexp.MustCompile(`Upload\.MaxFileSize\s*[<>]=?`)},
	{"config.Upload.UploadSize 裸比较", regexp.MustCompile(`Upload\.UploadSize\s*[<>]=?`)},
	{"config.Upload.TextMaxBytes 裸比较", regexp.MustCompile(`Upload\.TextMaxBytes\s*[<>]=?`)},
	{"config.Upload 容量旋钮读入变量", regexp.MustCompile(`=\s*[A-Za-z_.]*Upload\.(MaxFileSize|UploadSize|TextMaxBytes)`)},
}

// resourceScanRoots 层②③扫描根（相对 worktree）。pkg/ 也扫，但执行点自身
// resource.go 在扫描时单独跳过（它是判定的实现，不是散点）。
var resourceScanRoots = []string{"app", "bootstrap", "gen", "transport", "internal", "pkg"}

// resourceSkipFiles 扫描时跳过的文件（执行点自身）。
var resourceSkipFiles = map[string]bool{
	"pkg/gate/resource.go": true,
}

// resourceInlineExemptions 裸判定豁免登记：文件 → 理由（必须非空）。
// 起步为零（收口后无合法裸判定）；确有展示类只读用途时在此登记并写明理由。
var resourceInlineExemptions = map[string]string{
	"pkg/utils/filecheck.go": "访问器 GetTextShareMaxBytes 的缺省回退判定（cfg.Upload.TextMaxBytes <= 0 ⇒ 默认 222KB），" +
		"取值不是拒绝判定——容量拒绝一律经 pkg/gate.ExceedsTextShare",
	"app/config/policy.go": "保存期不变量校验的**取值**（读旋钮后交 gate.BatchCapBelowFileCap 判定，" +
		"判定本体已收口）；取值需区分 UploadEx 管理台持久化段与 conf 回退，不能走 gate 的全局读数",
}

// resourceUploadSurfaces 路由矩阵里 bucket=upload 的路由 → Resource 口径
// （站点承载文件，或 resourceNA + 理由）。**新增上传面必须在此登记**：要么声明
// 容量判定落在哪个承载文件，要么显式写明为什么不涉及容量。
var resourceUploadSurfaces = map[string]string{
	"POST /anonymous/generate":                         "gen/handler/share_anonymous/share_anonymous_service.go",
	"POST /anonymous/retrieve":                         resourceNA + "取件凭证面，无容量判定（次数/密码归 Pickup 面）",
	"POST /api/v1/federation/deliver":                  "transport/http/handler/federation_delivery.go",
	"DELETE /api/v1/mcp":                               resourceNA + "MCP 会话管理，无容量判定",
	"GET /api/v1/mcp":                                  resourceNA + "MCP 会话管理，无容量判定",
	"POST /api/v1/mcp":                                 "app/mcp/server.go",
	"POST /api/v1/presign/abort":                       resourceNA + "会话清理，无容量判定",
	"POST /api/v1/presign/complete":                    "app/presign/presign.go",
	"POST /api/v1/presign/upload":                      "app/presign/presign.go",
	"PUT /api/v1/presign/upload-direct/:uploadID":      "app/presign/presign.go",
	"POST /api/v1/request/:token/upload":               "transport/http/handler/file_request.go",
	"POST /api/v1/share/multi-bind":                    "transport/http/handler/share_multi.go",
	"POST /api/v1/share/multi-direct":                  "transport/http/handler/helpers.go",
	"DELETE /chunk/upload/cancel/:upload_id":           resourceNA + "会话取消，无容量判定",
	"POST /chunk/upload/chunk/:upload_id/:chunk_index": "app/chunk/service.go",
	"POST /chunk/upload/complete/:upload_id":           "gen/handler/chunk/chunk_service.go",
	"POST /chunk/upload/init/":                         "app/chunk/service.go",
	"GET /chunk/upload/status/:upload_id":              resourceNA + "会话状态查询，无容量判定",
	"POST /share/file/":                                "gen/handler/share/share_service.go",
	"POST /share/text/":                                "gen/handler/share/share_service.go",
}

// resourceNA 路由口径「不涉及容量判定」的声明前缀（必须跟理由）。
const resourceNA = "N/A："

// resourceCoreFiles 执行点核心自身的必含标记（防执行点被掏空）。
// Resource 面九个判定 + MinTaskInterval 在 resource.go；配额两闸的定义在
// gate.go / quota.go（#196 与 2026-10-03 前已收口），一并纳入核心自检。
var resourceCoreFiles = map[string][]string{
	"pkg/gate/resource.go": {
		"func MaxFileSize(", "func CheckFileSize(", "func ExceedsFileSize(",
		"func MaxRequestSize(", "func ExceedsRequestSize(",
		"func TextShareMaxBytes(", "func ExceedsTextShare(",
		"func HTTPBodyLimit(", "func CheckTaskBudget(",
		"func BatchCapBelowFileCap(",
		"const MinTaskInterval",
	},
	"pkg/gate/gate.go":  {"func CheckUploadSizeCap("},
	"pkg/gate/quota.go": {"func CheckAnonymousQuota("},
}

// resourceLoopFiles 允许出现 loop.Run( 的 bootstrap 源文件（必须过预算口）。
var resourceLoopFiles = map[string]bool{
	"bootstrap/tasks.go":           true, // startBGLoop 本体
	"bootstrap/federation_sync.go": true, // 热启停调度器（federationSyncApply 先过预算）
}

// stripLineComments 剥掉行注释（// 起到行尾），避免注释里的历史引用误报。
// 已知局限：同一行 // 之后的真代码也会被剥掉——本仓判定调用不写在 URL 之后。
func stripLineComments(src string) string {
	lines := strings.Split(src, "\n")
	for i, ln := range lines {
		if idx := strings.Index(ln, "//"); idx >= 0 {
			lines[i] = ln[:idx]
		}
	}
	return strings.Join(lines, "\n")
}

// resourceHasPattern 文件（剥注释后）是否命中任一裸判定模式。
func resourceHasPattern(content string) bool {
	src := stripLineComments(content)
	for _, p := range resourceBarePatterns {
		if strings.Contains(src, p) {
			return true
		}
	}
	for _, br := range resourceBareRe {
		if br.Re.MatchString(src) {
			return true
		}
	}
	return false
}

// resourceBareHits 文件命中的裸判定名（负对照用，逐条点名）。
func resourceBareHits(content string) []string {
	src := stripLineComments(content)
	var hits []string
	for _, p := range resourceBarePatterns {
		if strings.Contains(src, p) {
			hits = append(hits, p)
		}
	}
	for _, br := range resourceBareRe {
		if br.Re.MatchString(src) {
			hits = append(hits, br.Name)
		}
	}
	return hits
}

// resourceCallHit 文件是否调用资源判定函数（反向对账用）。
func resourceCallHit(content string) (string, bool) {
	src := stripLineComments(content)
	for _, name := range resourceFuncNames {
		if strings.Contains(src, "gate."+name+"(") {
			return name, true
		}
	}
	return "", false
}

// resourceWalk 扫描集：相对路径 → 内容（跳过测试文件与执行点自身）。
func resourceWalk(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, root := range resourceScanRoots {
		_ = filepath.Walk(filepath.Join("..", root), func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, rerr := filepath.Rel("..", path)
			if rerr != nil {
				return nil
			}
			slash := filepath.ToSlash(rel)
			if resourceSkipFiles[slash] {
				return nil
			}
			if b, rerr := os.ReadFile(path); rerr == nil {
				out[slash] = string(b)
			}
			return nil
		})
	}
	if len(out) == 0 {
		t.Fatalf("Resource 守卫扫描集为空（目录挪窝/走读失败）——守卫自身失效")
	}
	return out
}

// resourceCheckSurfaces 层①：矩阵 upload 桶 × 口径登记 × 承载文件存在。
func resourceCheckSurfaces(rows []routePolicy, surfaces map[string]string) []string {
	var probs []string
	uploadKeys := map[string]bool{}
	for _, row := range rows {
		if row.Bucket != "upload" {
			continue
		}
		uploadKeys[row.key()] = true
		site, ok := surfaces[row.key()]
		if !ok {
			probs = append(probs, "矩阵上传面 "+row.key()+
				"（bucket=upload）未在 resourceUploadSurfaces 登记 Resource 口径——新上传入口必须声明容量判定落在哪个承载文件，或显式 N/A：理由")
			continue
		}
		if strings.HasPrefix(site, resourceNA) {
			if strings.TrimSpace(strings.TrimPrefix(site, resourceNA)) == "" {
				probs = append(probs, "上传面 "+row.key()+
					" 的口径写成裸 N/A： 没有理由——不涉及容量判定也必须写明为什么")
			}
			continue
		}
		if _, ok := resourceSites[site]; !ok {
			probs = append(probs, "上传面 "+row.key()+
				" 登记的承载文件 "+site+" 不在 resourceSites（陈旧/笔误条目，先登记站点再引用）")
			continue
		}
	}
	for route := range surfaces {
		if !uploadKeys[route] {
			probs = append(probs, "resourceUploadSurfaces 登记陈旧："+route+" 不在矩阵（路由已删/改名，同步口径表）")
		}
	}
	return probs
}

// TestResourceEntryGuardCoverage 四查守卫主判据（见文件头）。
func TestResourceEntryGuardCoverage(t *testing.T) {
	files := resourceWalk(t)

	// ===== 层① 矩阵 upload 桶 × 口径登记 =====
	raw, err := os.ReadFile(policyMatrixPath(t))
	if err != nil {
		t.Fatalf("读取路由策略矩阵失败: %v", err)
	}
	rows, perr := parsePolicyMatrix(string(raw))
	if perr != nil {
		t.Fatalf("策略矩阵解析失败: %v", perr)
	}
	for _, p := range resourceCheckSurfaces(rows, resourceUploadSurfaces) {
		t.Error(p)
	}

	// ===== 层② 裸判定扫描 × 豁免 =====
	hitsByFile := map[string][]string{}
	for rel, content := range files {
		if hits := resourceBareHits(content); len(hits) > 0 {
			hitsByFile[rel] = hits
		}
	}
	for _, path := range sortedKeys(hitsByFile) {
		if reason, ok := resourceInlineExemptions[path]; !ok || strings.TrimSpace(reason) == "" {
			t.Errorf("层②：裸容量判定 %s %v（须改走 pkg/gate 执行点，或在 resourceInlineExemptions 登记理由）",
				path, hitsByFile[path])
		}
	}
	for _, path := range sortedKeys(resourceInlineExemptions) {
		if strings.TrimSpace(resourceInlineExemptions[path]) == "" {
			t.Errorf("层②：豁免 %s 缺理由", path)
		}
		if _, ok := hitsByFile[path]; !ok {
			t.Errorf("层②：豁免陈旧（%s 已无裸判定命中，迁移完成后删除条目）", path)
		}
	}

	// ===== 层③ 站点登记 × 承载标记 + 反向对账 =====
	for _, file := range sortedKeys(resourceSites) {
		b, err := os.ReadFile(filepath.Join("..", file))
		if err != nil {
			t.Errorf("层③：登记站点不可读 %s（文件被删/改名必须同步 resourceSites）", file)
			continue
		}
		content := string(b)
		for _, m := range resourceSites[file] {
			if n := strings.Count(content, m.Sub); n < m.Min {
				t.Errorf("层③：站点 %s 缺标记 %s（需 ≥%d 处，实得 %d）——承载文件删掉执行点调用",
					file, m.Sub, m.Min, n)
			}
		}
	}
	for _, rel := range sortedKeys(files) {
		if name, hit := resourceCallHit(files[rel]); hit {
			if _, ok := resourceSites[rel]; !ok {
				t.Errorf("层③：新站点 %s 调用了 gate.%s 但未登记 resourceSites（散点清单必须同步）", rel, name)
			}
		}
	}

	// ===== 层④ 执行点核心自检 + 后台循环预算口 =====
	for _, f := range sortedKeys(resourceCoreFiles) {
		b, err := os.ReadFile(filepath.Join("..", f))
		if err != nil {
			t.Errorf("层④：执行点核心文件 %s 不可读: %v", f, err)
			continue
		}
		content := string(b)
		for _, m := range resourceCoreFiles[f] {
			if !strings.Contains(content, m) {
				t.Errorf("层④：执行点被掏空——%s 缺 %q（判定函数被删/常量被移除）", f, m)
			}
		}
	}
	for _, rel := range sortedKeys(files) {
		if !strings.HasPrefix(rel, "bootstrap/") {
			continue // 后台任务预算只管装配层周期任务（有意不收项见 PR 盘点）
		}
		if strings.Contains(files[rel], "loop.Run(") && !resourceLoopFiles[rel] {
			t.Errorf("层④：%s 出现裸 loop.Run(——后台周期任务必须经 bootstrap.startBGLoop"+
				"（或调度器内先过 gate.CheckTaskBudget）", rel)
		}
		if strings.Contains(files[rel], "trackBackgroundLoop(loop.Run(") && !resourceLoopFiles[rel] {
			t.Errorf("层④：%s 直接 trackBackgroundLoop(loop.Run(——绕过预算启动口 startBGLoop", rel)
		}
	}
	for _, f := range sortedKeys(resourceLoopFiles) {
		content, ok := files[f]
		if !ok {
			t.Errorf("层④：预算口文件 %s 不在扫描集（挪窝/删除必须同步 resourceLoopFiles）", f)
			continue
		}
		if !strings.Contains(content, "gate.CheckTaskBudget(") {
			t.Errorf("层④：预算口文件 %s 缺少 gate.CheckTaskBudget(——后台任务预算判定被摘（core#173 第六片）", f)
		}
	}

	if !t.Failed() {
		uploadRows := 0
		for _, r := range rows {
			if r.Bucket == "upload" {
				uploadRows++
			}
		}
		t.Logf("Resource 收口守卫通过：%d 条路由（%d 个上传面）、%d 个判定站点、%d 个裸判据豁免、%d 个扫描文件",
			len(rows), uploadRows, len(resourceSites), len(resourceInlineExemptions), len(files))
	}
}

// TestResourceEntryGuardNegativeControl 合成输入自证四查有牙齿（常态 CI 恒绿）。
// 每一维都必须「破坏 ⇒ 点名」，防止判定逻辑空转（正向永远绿）。
func TestResourceEntryGuardNegativeControl(t *testing.T) {
	// 层①：未登记上传面 / 幽灵承载 / 裸 N/A / 登记陈旧
	baseRows := []routePolicy{
		{Method: "POST", Path: "/share/file/", Bucket: "upload"},
		{Method: "GET", Path: "/admin/x", Bucket: "global"},
	}
	if probs := resourceCheckSurfaces(baseRows, map[string]string{}); len(probs) == 0 {
		t.Errorf("层①负对照失败：未登记上传面未被点名")
	}
	if probs := resourceCheckSurfaces(baseRows, map[string]string{"POST /share/file/": "ghost.go"}); !hasSubstring(probs, "不在 resourceSites") {
		t.Errorf("层①负对照失败：幽灵承载未被点名（probs=%v）", probs)
	}
	if probs := resourceCheckSurfaces(baseRows, map[string]string{"POST /share/file/": resourceNA}); !hasSubstring(probs, "没有理由") {
		t.Errorf("层①负对照失败：无理由 N/A 未被点名（probs=%v）", probs)
	}
	if probs := resourceCheckSurfaces(baseRows, map[string]string{
		"POST /share/file/": "app/chunk/service.go",
		"POST /ghost/":      "app/chunk/service.go",
	}); !hasSubstring(probs, "登记陈旧") {
		t.Errorf("层①负对照失败：陈旧口径未被点名（probs=%v）", probs)
	}

	// 层②：裸模式命中 / 执行点调用不误伤 / 注释引用不误伤 / 裸比较命中
	if hits := resourceBareHits("limit := utils.GetMaxFileSize()"); len(hits) == 0 {
		t.Errorf("层②负对照失败：裸模式应命中")
	}
	if hits := resourceBareHits("maxFile := config.Upload.MaxFileSize"); len(hits) == 0 {
		t.Errorf("层②负对照失败：容量旋钮读入变量应命中（hits=%v）", hits)
	}
	if hits := resourceBareHits("if config.Upload.UploadSize > n {"); len(hits) == 0 {
		t.Errorf("层②负对照失败：config 裸比较应命中（hits=%v）", hits)
	}
	if hits := resourceBareHits("gate.CheckFileSize(size)"); len(hits) != 0 {
		t.Errorf("层②负对照失败：执行点调用被误伤（hits=%v）", hits)
	}
	if hits := resourceBareHits("// 历史上用 utils.GetMaxFileSize() 手写比较"); len(hits) != 0 {
		t.Errorf("层②负对照失败：注释引用被误伤（hits=%v）", hits)
	}

	// 层③：反向对账识别资源/配额调用；非资源函数不误判
	if name, hit := resourceCallHit("gate.HTTPBodyLimit()"); !hit || name != "HTTPBodyLimit" {
		t.Errorf("层③负对照失败：反向对账应识别 HTTPBodyLimit（name=%q hit=%v）", name, hit)
	}
	if name, hit := resourceCallHit("gate.CheckAnonymousQuota(ctx, ip, n)"); !hit || name != "CheckAnonymousQuota" {
		t.Errorf("层③负对照失败：配额闸应纳入反向对账（name=%q hit=%v）", name, hit)
	}
	if _, hit := resourceCallHit("gate.PrecheckPickup(x)"); hit {
		t.Errorf("层③负对照失败：非资源函数被误判为资源站点")
	}

	// 层④：核心名单非空且形态正确（防名单被清空导致层④空转）+ 循环白名单
	if len(resourceFuncNames) != 12 {
		t.Errorf("层④负对照失败：资源判定函数名单应 12 个，实得 %d（名单被清空 ⇒ 层③④空转）", len(resourceFuncNames))
	}
	if !resourceLoopFiles["bootstrap/tasks.go"] || !resourceLoopFiles["bootstrap/federation_sync.go"] {
		t.Errorf("层④负对照失败：预算口白名单被清空 ⇒ 循环检查空转")
	}
}

// hasSubstring 子串命中（负对照断言用；与 authz 守卫的 hasProb 同义）。
func hasSubstring(items []string, sub string) bool {
	for _, it := range items {
		if strings.Contains(it, sub) {
			return true
		}
	}
	return false
}
