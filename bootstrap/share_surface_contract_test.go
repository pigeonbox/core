// 取件闸门契约守卫（2026-10-24）
//
// ## 为什么有这道门禁
//
// `model.FileCode.IsExpired()` 拆分前**含次数语义**（`ExpiredCount==0 ⇒ 过期`）
// ⇒ 凡是以它为闸的门，此前**同时**挡着「次数耗尽」；拆分后各面只判时间，
// **次数闸就漏了**。实测漏了 4 个面（core v0.32.4 修复）：
//
//	/share/metadata、/anonymous/search  → 仍回显文件名/大小
//	MCP get_share_content / download_share_file → 仍回传正文/文件内容
//
// 漏闸的根因不是「某个人忘了写」，而是**闸门组合是每个面各自手搓的**：
// 新加一个面时，没有任何机制要求作者声明「这个面交不交付内容、次数耗尽时
// 怎么判」⇒ 漏闸是**默认结果**，不是意外。
//
// ⇒ 本门禁把「每个面向外交付什么 + 次数耗尽时怎么判」变成**必须显式声明**的
// 契约表，并从**真实注册路由**与 **MCP 工具注册表**双向核对：
//
//	① 表内每条路由必须真实存在（不许留陈旧条目）；
//	② 真实注册表里凡是「分享面向外」的路由，都必须在表内（新面必须声明）；
//	③ MCP 侧「读分享内容」的工具（注册表标注 ReadsShare）必须在表内；
//	④ 表内声明的覆盖测试函数必须**真实存在**（不许写空头承诺）——用源码树
//	   全文检索机械校验，测试被删/改名 ⇒ 守卫变红。
//
// 断言必须能变红（负对照）：拿掉表里任意一条 ⇒ ②/③ 红；改掉一个测试函数名
// ⇒ ④ 红；新增一个 /share/ 下的路由而不声明 ⇒ ② 红。
package bootstrap

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"

	"github.com/pigeonbox/contracts/errcode"
	mcpApp "github.com/pigeonbox/core/app/mcp"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/gen/router"
	"github.com/pigeonbox/core/pkg/resp"
)

// shareSurfaceContract 一个「分享面向外」的面在取件闸门上的契约。
//
// Surface  面标识（HTTP: "GET /share/metadata/:code"；MCP: "MCP get_share_content"）
// Delivers 面交付什么（创建/凭证/元信息/正文/文件内容）
// Gate     次数耗尽时的判定口径（N/A=不涉及已有分享的取件闸）
// CoveredBy 覆盖该面的行为测试函数名（空 ⇒ 该面无需行为测试，见 Gate 说明）
type shareSurfaceContract struct {
	Surface   string
	Delivers  string
	Gate      string
	CoveredBy string
	// Code 该面在「次数耗尽」时返回的**业务码**（0 = 该面不涉及业务码判定）。
	// HTTP 该业务码对应的 **HTTP 状态码**（0 = 不校验）。
	//
	// 为什么要登记 HTTP 状态（2026-10-25 审计 A9）：此前表里只有业务码 ⇒
	// 「同语义不同 HTTP 状态」（匿名 20007=200 vs 分享 20004=403）这类退化
	// **结构上无法被守卫发现**。⇒ 登记后由守卫核对「表里的 HTTP 状态」与
	// 「运行时实际映射（resp.HTTPStatusForCode）」一致。
	Code int
	HTTP int
}

// shareSurfaceContracts 取件闸门契约表（**新增面向外交付的面必须在此登记**）。
//
// Gate 取值约定：
//   - "N/A"：创建面（新建分享，次数由 owner 设定，尚未产生取件）；
//   - "20004"：分享通道（/share/*、/preview）——超过取件次数；
//   - "20007"：匿名通道（/anonymous/*）——取件码已用完；
//   - "交付点扣次"：闸门在**内容交付那一刻**（/share/download）——
//     授权阶段不拦（否则会误伤「已扣次的那一次取件仍可完成交付」）；
//   - "豁免"：管理面元信息（与 /admin/files 同规格，不受对外分发配额约束）。
var shareSurfaceContracts = []shareSurfaceContract{
	// ===== 创建面（新分享，尚无取件行为）=====
	{"POST /share/text/", "创建文本分享", "N/A", "", 0, 0},
	{"POST /share/file/", "创建文件分享", "N/A", "", 0, 0},
	{"POST /anonymous/generate", "铸造取件码（占位）", "N/A", "", 0, 0},
	{"POST /api/v1/share/multi-direct", "创建多文件分享", "N/A", "", 0, 0},
	{"POST /api/v1/share/multi-bind", "绑定多文件分享", "N/A", "", 0, 0},

	// ===== 交付面（读已有分享）=====
	{"GET /share/select/", "文本正文（交付面）", "20004", "TestSelect_CountExhausted_ReturnsForbidden", errcode.CodeShareReachLimit, 403},
	{"GET /share/download", "文件内容（交付面）", "交付点扣次", "TestDownload_CountExhausted_BlocksNewSession_AllowsChargedSession", errcode.CodeShareReachLimit, 403},
	{"GET /share/metadata/:code", "分享元信息（文件名/大小）", "20004", "TestShareMetadata_CountExhaustedReturns20004", errcode.CodeShareReachLimit, 403},
	{"GET /preview/:code", "预览摘要（内容派生）", "20004", "TestPreview_CountExhausted_ReturnsReachLimit", errcode.CodeShareReachLimit, 403},
	{"POST /anonymous/retrieve", "取件凭证（不扣次）", "20007", "TestRetrieve_CountExhausted", errcode.CodePickupCodeExhausted, 403},
	{"GET /anonymous/download/:code", "下载直链（302）", "20007", "TestAnonymousDownload_CountExhausted_ReturnsExhausted", errcode.CodePickupCodeExhausted, 403},
	{"GET /anonymous/search/:code", "分享元信息（文件名/大小）", "20007", "TestSearchByCode_CountExhausted", errcode.CodePickupCodeExhausted, 403},

	// ===== MCP 工具（读已有分享）=====
	{"MCP get_share", "分享元信息（管理面）", "豁免", "", 0, 0},
	{"MCP preview_share", "预览摘要（文本内容派生）", "20004", "TestPreview_CountExhausted_ReturnsReachLimit", errcode.CodeShareReachLimit, 403},
	{"MCP get_share_content", "文本正文 / 文件清单（管理面）", "取件次数已用完", "TestToolsCall_CountExhaustedShareDenied", 0, 0},
	{"MCP download_share_file", "文件内容（管理面）", "取件次数已用完", "TestToolsCall_CountExhaustedFileShareDenied", 0, 0},
}

// shareSurfacePatterns 判定「分享面向外」的路由模式。
//
// 覆盖：分享通道（/share/*）、匿名取件通道（/anonymous/*）、预览面（/preview/*）。
// **新增这类前缀下的路由即自动进入契约核对** ⇒ 忘登记 ⇒ 守卫变红。
var shareSurfacePatterns = []string{"/share/", "/anonymous/", "/preview/"}

// TestShareSurfaceContractCoverage 取件闸门契约守卫（双向 + 覆盖测试存在性）。
func TestShareSurfaceContractCoverage(t *testing.T) {
	prev := config
	config = &conf.AppConfiguration{
		MCP:      conf.MCPConfig{Enabled: true},
		Security: conf.SecurityConfig{OIDC: conf.OIDCConfig{Enabled: true}},
		UI:       conf.UIConfig{ExposeOpenAPI: true},
	}
	defer func() { config = prev }()

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	router.GeneratedRegister(h)
	customizedRegister(h)

	// 真实注册表里「分享面向外」的面
	real := map[string]bool{}
	for _, r := range h.Routes() {
		key := r.Method + " " + r.Path
		for _, p := range shareSurfacePatterns {
			if strings.Contains(r.Path, p) {
				real[key] = true
				break
			}
		}
	}
	// MCP 侧读分享内容的工具：
	//   · 作者自标（Gate.ReadsShare）—— 声明「我交付内容」；
	//   · 客观判据（出参含内容字段）—— 反射推导，**不依赖作者自觉**。
	// 两者取并集进核对集合 ⇒ 作者忘自标时，客观判据仍会把它捞进来
	// （2026-10-25 审计 A2：守卫的发现机制自身有洞 ⇒ preview_share 漏出契约表）。
	for _, name := range mcpApp.ShareReadingToolNames() {
		real["MCP "+name] = true
	}
	for _, name := range mcpApp.ContentDeliveringToolNames() {
		real["MCP "+name] = true
		// 客观判定为交付内容、但作者没自标 ⇒ 自标与客观判据不一致 ⇒ 报出来
		// （不直接判失败：可能是「管理面豁免」的有意为之，但必须显式登记）
		if mcpApp.ShareGatePolicy(name) == nil {
			t.Errorf("MCP 工具 %q 的出参含内容字段（客观判定为交付内容）但未在注册表声明 Gate —— 守卫的发现机制不能只信作者自标；请补 Gate 声明或说明其为管理面豁免", name)
		}
	}

	// 契约表索引
	declared := map[string]shareSurfaceContract{}
	for _, c := range shareSurfaceContracts {
		if _, dup := declared[c.Surface]; dup {
			t.Errorf("契约表重复登记 %s", c.Surface)
		}
		declared[c.Surface] = c
	}

	// ① 表内每条路由必须真实存在（MCP 工具由注册表单独核对）
	for _, c := range shareSurfaceContracts {
		if strings.HasPrefix(c.Surface, "MCP ") {
			tool := strings.TrimPrefix(c.Surface, "MCP ")
			policy := mcpApp.ShareGatePolicy(tool)
			if policy == nil || !policy.ReadsShare {
				t.Errorf("契约表登记的 MCP 工具 %q 不存在，或未被标注为读分享内容（ReadsShare）", tool)
				continue
			}
			if policy.Exhausted != c.Gate {
				t.Errorf("MCP 工具 %q 的耗尽判定与注册表标注不一致：契约表=%q 注册表=%q", tool, c.Gate, policy.Exhausted)
			}
			continue
		}
		if !real[c.Surface] {
			t.Errorf("契约表登记的面 %q 在真实路由表里不存在（陈旧条目）", c.Surface)
		}
	}

	// ② + ③ 真实注册表里「分享面向外」的面必须都在契约表内
	var missing []string
	for s := range real {
		if _, ok := declared[s]; !ok {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		t.Errorf("新增了 %d 个分享面向外的面但未在取件闸门契约表登记（必须声明「交不交付内容/次数耗尽时怎么判/由哪个测试覆盖」）:\n  %s",
			len(missing), joinLines(missing))
	}

	// ④b 表内登记的 HTTP 状态必须与运行时映射一致（A9：状态码退化可被发现）
	for _, c := range shareSurfaceContracts {
		if c.Code == 0 || c.HTTP == 0 {
			continue
		}
		if got := resp.HTTPStatusForCode(c.Code); got != c.HTTP {
			t.Errorf("面 %s 登记的 HTTP 状态 %d 与运行时映射（业务码 %d → %d）不一致——同语义不同状态码的退化必须被发现",
				c.Surface, c.HTTP, c.Code, got)
		}
	}

	// ④ 契约表声明的覆盖测试函数必须真实存在（源码树全文检索）
	srcRoot := ".."
	for _, c := range shareSurfaceContracts {
		if c.CoveredBy == "" {
			continue
		}
		if !testFuncExists(srcRoot, c.CoveredBy) {
			t.Errorf("契约表声明的覆盖测试 %q（面 %s）在源码树里不存在——测试被删/改名会让这里的承诺落空", c.CoveredBy, c.Surface)
		}
	}
}

// testFuncExists 在源码树里检索测试函数定义 `func <name>(`。
func testFuncExists(root, name string) bool {
	pat := regexp.MustCompile(`(?m)^func ` + regexp.QuoteMeta(name) + `\(`)
	found := false
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || found || info == nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		if pat.Match(b) {
			found = true
		}
		return nil
	})
	return found
}

// TestShareSurfaceContractGateValues 契约表的闸门口径取值必须是受控词表
// （防止有人写「差不多」的口径，机械不可判定）。
func TestShareSurfaceContractGateValues(t *testing.T) {
	allowed := map[string]bool{
		"N/A": true, "20004": true, "20007": true,
		"交付点扣次": true, "豁免": true, "取件次数已用完": true,
	}
	for _, c := range shareSurfaceContracts {
		if !allowed[c.Gate] {
			t.Errorf("面 %s 的闸门口径 %q 不在受控词表内（允许：%s）", c.Surface, c.Gate, "N/A / 20004 / 20007 / 交付点扣次 / 豁免 / 取件次数已用完")
		}
		if c.Delivers == "" {
			t.Errorf("面 %s 未声明交付什么", c.Surface)
		}
	}
}
