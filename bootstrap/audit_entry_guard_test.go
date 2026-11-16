// 审计执行点收口守卫（core#173 第七片：Audit Policy 收口）。
//
// ## 为什么有这道门禁
//
// pkg/audit.Record 是关键操作记录（admin_operation_logs）的单一执行点，但
// **没有任何机制要求新入口调用它**——历史上 admin 域与 config 域各持一份
// 「提取操作者 + 构造模型 + Create + 失败告警」的孪生实现（连告警文案都不
// 一致），新 handler 照抄即可绕过收口点，且没有任何测试会红。
//
// ⇒ 本守卫把「关键操作记录只准走 pkg/audit.Record」变成机械判据（三层，与
// authz_entry_guard 同款）：
//
//	① 写入站点登记 × 承载标记：已知写入面（admin 域入口、config 域入口、
//	   gen admin 审计委托、p2p gen 写者、bootstrap 装配与 dao 桥、wiring 接线）
//	   必须登记在 auditSites，且承载文件含执行点调用标记；执行点核心
//	   （pkg/audit 的 Record/SetSink/Sink 定义）缺失 ⇒ 掏空红；
//	② 源码扫描 × 裸写豁免：app/bootstrap/gen/transport/pkg/repo 的非测试源码
//	   出现裸构造 model.AdminOperationLog 或绕过执行点直写仓储 ⇒ 红，除非在
//	   auditInlineExemptions 登记理由；豁免条目已无标记 ⇒ 陈旧红；
//	③ 反向对账：源码出现执行点调用形态（audit. / adminapp.Audit( / .LogOperation
//	   / SetAuditWriter）但文件未在登记集 ⇒ 红（新站点必须进清单，PR 散点
//	   清单随之更新）。
//
// 负对照：TestAuditEntryGuardNegativeControl 用合成输入自证三层判定有牙齿
// （恒绿）；真实文件级负对照（摘 app/config/service.go 的 audit.Record 调用 ⇒
// ①③ 红、还原 ⇒ 绿；handler 裸构造 model.AdminOperationLog ⇒ ② 红）见 PR
// 正文实跑记录。
//
// 行为级兜底：执行点被掏空（如 Record 恒不落盘）由 pkg/audit 单测与既有
// 审计相关测试覆盖。
package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// auditMarker 承载文件的必含标记：Sub 在文件中至少出现 Min 次。
type auditMarker struct {
	Sub string
	Min int
}

// auditSite 一个必须经执行点的写入站点（承载文件 + 必含执行点调用标记）。
type auditSite struct {
	File    string
	Markers []auditMarker
}

// auditSites 写入站点登记（= PR 散点迁移清单的机械对账面）。
// 新增关键操作记录站点必须登记；站点文件删掉执行点调用 ⇒ 红。
var auditSites = map[string]auditSite{
	"admin 域操作日志入口": {File: "app/admin/service.go",
		Markers: []auditMarker{{Sub: "audit.Record(", Min: 1}}},
	"config 域操作日志入口": {File: "app/config/service.go",
		Markers: []auditMarker{{Sub: "audit.Record(", Min: 1}}},
	"gen admin 审计委托": {File: "gen/handler/admin/admin_manage.go",
		Markers: []auditMarker{{Sub: "adminapp.Audit(", Min: 1}}},
	"p2p gen 审计写者": {File: "gen/handler/p2p/admin_p2_pservice.go",
		Markers: []auditMarker{{Sub: "auditWriter(ctx, action, target, err == nil)", Min: 1}}},
	"p2p 审计写者接线": {File: "bootstrap/wiring.go",
		Markers: []auditMarker{{Sub: "SetAuditWriter(adminApp.Default().LogOperation)", Min: 1}}},
	"bootstrap sink 装配": {File: "bootstrap/bootstrap.go",
		Markers: []auditMarker{{Sub: "audit.SetSink(", Min: 1}}},
	"离线备份 sink 装配": {File: "bootstrap/backup.go",
		Markers: []auditMarker{{Sub: "audit.SetSink(daoAuditSink{db: database})", Min: 1}}},
	"dao 桥（Entry→模型）": {File: "bootstrap/adapters.go",
		Markers: []auditMarker{{Sub: "model.AdminOperationLog{", Min: 1}, {Sub: "audit.Entry", Min: 1}}},
}

// auditCoreFiles 执行点核心自身的必含标记（防 pkg/audit 被掏空/函数被删）。
var auditCoreFiles = map[string][]auditMarker{
	"pkg/audit/audit.go": {
		{Sub: "func Record(", Min: 1},
		{Sub: "func SetSink(", Min: 1},
		{Sub: "type Sink interface", Min: 1},
	},
}

// auditBareRes 裸写扫描标记（层②）：只拦「绕过执行点的落库形态」——
// 直接构造模型、直接调仓储 Create。查询（List/DeleteOlderThan）与表注册/
// AutoMigrate 不在此列。
var auditBareRes = []struct {
	Name string
	Re   *regexp.Regexp
}{
	{"裸构造AdminOperationLog", regexp.MustCompile(`model\.AdminOperationLog\{`)},
	{"绕过执行点直写(字段仓储)", regexp.MustCompile(`adminOperationRepo\.Create\(`)},
	{"绕过执行点直写(构造式仓储)", regexp.MustCompile(`NewAdminOperationLogRepository\([^)]*\)\.Create\(`)},
}

// auditScanDirs 层②扫描范围（相对仓根；pkg/audit 执行点自身也扫——它不得
// 依赖 repo 层构造模型，谁往里塞就该红）。
var auditScanDirs = []string{"app", "bootstrap", "gen", "transport", "pkg", "repo"}

// auditInlineExemptions 层②豁免：文件 → 理由。
// 豁免只给「执行点的落盘链本身」（dao 桥与仓储实现），不给业务侧裸写开后门。
var auditInlineExemptions = map[string]string{
	"bootstrap/adapters.go":              "pkg/audit.Sink 的 dao 桥（composition root 装配层构造模型，全站唯一允许的构造点）",
	"repo/db/dao/admin_operation_log.go": "dao 仓储定义（Create/List/DeleteOlderThan，执行点 Sink 的落盘实现）",
}

// auditReverseRes 层③反向对账模式：源码出现任一形态即必须登记。
var auditReverseRes = []*regexp.Regexp{
	regexp.MustCompile(`audit\.`),
	regexp.MustCompile(`adminapp\.Audit\(`),
	regexp.MustCompile(`\.LogOperation`),
	regexp.MustCompile(`SetAuditWriter`),
}

// ===== 推导层 =====

// auditScanFiles 层②扫描集：相对路径 → 内容（跳过测试文件）。
func auditScanFiles(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, dir := range auditScanDirs {
		root := filepath.Join("..", dir)
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, rerr := filepath.Rel("..", path)
			if rerr != nil {
				return nil
			}
			if data, rerr := os.ReadFile(path); rerr == nil {
				out[filepath.ToSlash(rel)] = string(data)
			}
			return nil
		})
	}
	if len(out) == 0 {
		t.Fatalf("层②扫描集为空（目录挪窝/走读失败）——守卫自身失效")
	}
	return out
}

// auditRegistered 反向对账的登记集（站点承载 + 执行点核心）。
func auditRegistered() map[string]bool {
	registered := map[string]bool{}
	for _, s := range auditSites {
		registered[s.File] = true
	}
	for f := range auditCoreFiles {
		registered[f] = true
	}
	return registered
}

// ===== 判定层（纯函数，供主守卫与负对照共用）=====

// auditCheckSites 层①③：站点/核心标记（正向）+ 反向对账。
func auditCheckSites(files map[string]string, read func(string) (string, error)) []string {
	var probs []string
	// 正向：登记站点与执行点核心的承载文件必须含执行点调用标记。
	agg := map[string][]auditMarker{}
	for _, name := range sortedKeys(auditSites) {
		s := auditSites[name]
		agg[s.File] = append(agg[s.File], s.Markers...)
	}
	for f, ms := range auditCoreFiles {
		agg[f] = append(agg[f], ms...)
	}
	for _, f := range sortedKeys(agg) {
		content, err := read(f)
		if err != nil {
			probs = append(probs, fmt.Sprintf("执行点承载文件 %s 读取失败: %v", f, err))
			continue
		}
		for _, m := range agg[f] {
			if strings.Count(content, m.Sub) < m.Min {
				probs = append(probs, fmt.Sprintf(
					"执行点承载文件 %s 缺少标记 %q（需 ≥%d 处）——记录调用被删或绕过 pkg/audit 执行点（core#173 第七片）",
					f, m.Sub, m.Min))
			}
		}
	}
	// 反向：源码出现执行点调用形态但文件未登记 ⇒ 新站点必须进清单。
	registered := auditRegistered()
	for _, f := range sortedKeys(files) {
		var hit []string
		for _, re := range auditReverseRes {
			if re.MatchString(files[f]) {
				hit = append(hit, re.String())
			}
		}
		if len(hit) == 0 {
			continue
		}
		if !registered[f] {
			probs = append(probs, fmt.Sprintf(
				"源文件 %s 出现审计执行点调用形态 %v 但未在 auditSites/auditCoreFiles 登记——新写入站点必须进清单（同步 PR 散点清单）",
				f, hit))
		}
	}
	return probs
}

// auditScanBareWrites 层②：裸写 × 豁免清单（双向对账）。
func auditScanBareWrites(files map[string]string, exemptions map[string]string) []string {
	var probs []string
	marked := map[string][]string{}
	for _, path := range sortedKeys(files) {
		var hits []string
		for _, br := range auditBareRes {
			if br.Re.MatchString(files[path]) {
				hits = append(hits, br.Name)
			}
		}
		if len(hits) > 0 {
			marked[path] = hits
		}
	}
	for _, path := range sortedKeys(marked) {
		if _, ok := exemptions[path]; !ok {
			probs = append(probs, fmt.Sprintf(
				"源文件 %s 出现审计裸写 %v——关键操作记录必须经 pkg/audit.Record 执行点"+
					"（Sink 落盘链见 bootstrap/adapters.go），或在 auditInlineExemptions 登记理由",
				path, marked[path]))
		}
	}
	for _, path := range sortedKeys(exemptions) {
		if _, ok := marked[path]; !ok {
			probs = append(probs, fmt.Sprintf(
				"auditInlineExemptions 登记陈旧：%s 已无裸写标记（迁移完成后删除豁免条目）", path))
		}
	}
	return probs
}

// ===== 守卫 =====

// TestAuditEntryGuardCoverage 审计执行点收口守卫（三层，见文件头）。
func TestAuditEntryGuardCoverage(t *testing.T) {
	read := func(rel string) (string, error) {
		b, err := os.ReadFile(filepath.Join("..", rel))
		return string(b), err
	}
	files := auditScanFiles(t)

	var probs []string
	// ① 站点/核心登记 × 承载标记；③ 反向对账
	probs = append(probs, auditCheckSites(files, read)...)
	// ② 源码扫描 × 豁免清单
	probs = append(probs, auditScanBareWrites(files, auditInlineExemptions)...)

	for _, p := range probs {
		t.Error(p)
	}
	if len(probs) == 0 {
		t.Logf("审计收口守卫通过：%d 个写入站点、%d 个核心文件、%d 个豁免、扫描 %d 个源文件",
			len(auditSites), len(auditCoreFiles), len(auditInlineExemptions), len(files))
	}
}

// TestAuditEntryGuardNegativeControl 负对照（自证三层判定有牙齿）：
// 合成输入逐维破坏 ⇒ 对应层必报且点名；基准输入 ⇒ 零问题。
// 常态 CI 恒绿——它验证判定逻辑本身；真实文件级负对照见 PR 正文。
func TestAuditEntryGuardNegativeControl(t *testing.T) {
	// 真实文件回退读取（正向判定作用于全部登记站点；桩只覆盖被破坏的那一个）。
	realRead := func(rel string) (string, error) {
		b, err := os.ReadFile(filepath.Join("..", rel))
		return string(b), err
	}
	stubRead := func(stub map[string]string) func(string) (string, error) {
		return func(rel string) (string, error) {
			if v, ok := stub[rel]; ok {
				return v, nil
			}
			return realRead(rel)
		}
	}

	// 基准：真实登记集 + 执行点调用齐全 ⇒ 正向零问题
	if probs := auditCheckSites(map[string]string{}, realRead); len(probs) != 0 {
		t.Fatalf("基准站点输入应零问题，实际: %v", probs)
	}
	// 基准：豁免覆盖的落盘链裸写 ⇒ 零问题
	baseBare := map[string]string{
		"bootstrap/adapters.go":              "func (daoAuditSink) Create(ctx context.Context, e audit.Entry) error {\n\treturn repo.Create(ctx, &model.AdminOperationLog{})\n}\n",
		"repo/db/dao/admin_operation_log.go": "func (r *AdminOperationLogRepository) Create(ctx context.Context, log *model.AdminOperationLog) error {\n\treturn r.db().Model(&model.AdminOperationLog{}).Create(log).Error\n}\n",
	}
	if probs := auditScanBareWrites(baseBare, auditInlineExemptions); len(probs) != 0 {
		t.Fatalf("基准裸写输入应零问题（豁免覆盖），实际: %v", probs)
	}

	// ①a 站点承载文件删掉 audit.Record 调用 ⇒ 红
	broken := stubRead(map[string]string{
		"app/admin/service.go": "package admin\n// no call here\n",
	})
	if probs := auditCheckSites(map[string]string{}, broken); !hasProb(probs, "缺少标记") || !hasProb(probs, "app/admin/service.go") {
		t.Errorf("负对照①a失败：站点标记缺失未被点名（probs=%v）", probs)
	}
	// ①b 执行点核心被掏空（Record 定义被删）⇒ 红
	hollow := stubRead(map[string]string{
		"pkg/audit/audit.go": "package audit\n// Record removed\n",
	})
	if probs := auditCheckSites(map[string]string{}, hollow); !hasProb(probs, "func Record(") || !hasProb(probs, "pkg/audit/audit.go") {
		t.Errorf("负对照①b失败：执行点核心掏空未被点名（probs=%v）", probs)
	}

	// ②a 裸写未豁免 ⇒ 红
	bare := map[string]string{"app/new/thing.go": "entry := &model.AdminOperationLog{Action: a}\n"}
	if probs := auditScanBareWrites(bare, map[string]string{}); !hasProb(probs, "审计裸写") || !hasProb(probs, "app/new/thing.go") {
		t.Errorf("负对照②a失败：裸写未被点名（probs=%v）", probs)
	}
	// ②b 豁免陈旧 ⇒ 红
	stale := map[string]string{"app/new/clean.go": "package clean\n"}
	if probs := auditScanBareWrites(stale, map[string]string{"app/new/clean.go": "历史理由"}); !hasProb(probs, "登记陈旧") {
		t.Errorf("负对照②b失败：陈旧豁免未被点名（probs=%v）", probs)
	}

	// ③ 反向：出现执行点调用但未登记 ⇒ 红
	unreg := map[string]string{"app/new/svc.go": "audit.Record(ctx, a, t, true)\n"}
	if probs := auditCheckSites(unreg, stubRead(unreg)); !hasProb(probs, "未在 auditSites/auditCoreFiles 登记") || !hasProb(probs, "app/new/svc.go") {
		t.Errorf("负对照③a失败：未登记调用文件未被点名（probs=%v）", probs)
	}
}
