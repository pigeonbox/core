package config

// policy.go 策略模板层（hub#368）：把分散的匿名/口令/存储/有效期旋钮收敛为
// 命名模板，一次下发多配置项。
//
// 设计边界（刻意不动的东西）：
//   - **不改任何单旋钮语义**：模板只是 SystemConfig 局部 JSON 的命名集合，
//     应用走既有 UpdateConfig 通道（写库 → overlay → OnOverlayApplied 生效链）。
//   - **不新增端点、不动 contracts**：清单经 GET /admin/config 的宽松响应面
//     下发（policy_templates 顶层键），应用经 PUT /admin/config 的
//     {"policy_template":"<id>"} 单键提交（handler 分流，未知键天然不落库）。
//   - **一致性校验在保存时拒绝**（ErrPolicyInvalid → 400），而不是等运行期
//     413 才暴露——复用管理台既有的「对齐/告警」机制做修复路径。
//
// 一致性校验为什么只挂在模板应用路径：单旋钮保存路径当前允许「先存矛盾态、
// 管理台告警 + 一键对齐」的既有流程（2026-10-30 起的 UI 判据），本层不改那个
// 行为，只保证**模板组合**不在保存时把站点带进矛盾态。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/gate"
)

// ErrPolicyInvalid 策略模板类可操作错误（未知模板 / 模板组合违反不变量）。
// handler 据此回 400 + 10001 + 具体文案（点名参数类错误回可操作文案的既有口径）；
// DB/基建错误不包此哨兵，仍走 500 脱敏出口。
var ErrPolicyInvalid = errors.New("策略模板被拒绝")

// PolicyTemplate 策略模板：一组配置项的命名集合。
type PolicyTemplate struct {
	// ID 稳定标识（API/前端 i18n 按它映射；导出 JSON 亦用它）。
	ID string `json:"id"`
	// Name 中文展示名（API 直读/导出用；管理台五语按 ID 走前端语言包，
	// 缺键时回退本字段）。
	Name string `json:"name"`
	// Description 中文一句话说明（同上，前端缺键时回退）。
	Description string `json:"description"`
	// Patch SystemConfig 局部 JSON：只含本模板要下发的字段，
	// 未提及的字段/段保留现状（字段级合并，语义同 PUT /admin/config）。
	Patch json.RawMessage `json:"patch"`
}

// builtinPolicyTemplates 内置模板清单（真相源：hub#368 正文的三个预设）。
//
// 每个模板的取值都必须**自身满足跨配置不变量**（见 ValidateConfigInvariants），
// 否则「干净站点上应用该模板」都会被拒——policy_test.go 逐个钉住。
// 改模板取值前先跑 TestPolicyTemplates_SelfConsistent。
var builtinPolicyTemplates = []PolicyTemplate{
	{
		ID:          "intranet",
		Name:        "纯内网",
		Description: "可信内网部署：放开访客上传与日配额、关闭自助注册、下载不需登录、默认 7 天/100 次",
		Patch: json.RawMessage(`{
			"upload_ex": {"open_upload": true, "anonymous_daily_count": 0, "anonymous_daily_bytes": 0},
			"user": {"allowuserregistration": false},
			"download": {"require_login": false},
			"transfer": {"max_count": 100, "expire_default": 7}
		}`),
	},
	{
		ID:          "public-no-anonymous",
		Name:        "公开但禁匿名",
		Description: "对公网开放的柜子：访客不可上传（须登录）、开放自助注册、下载不需登录、默认 3 天/50 次",
		Patch: json.RawMessage(`{
			"upload_ex": {"open_upload": false, "anonymous_daily_count": 0, "anonymous_daily_bytes": 0},
			"user": {"allowuserregistration": true},
			"download": {"require_login": false},
			"transfer": {"max_count": 50, "expire_default": 3}
		}`),
	},
	{
		ID:          "enterprise",
		Name:        "企业默认",
		Description: "受控部署：访客禁传、禁自助注册、存储配额 10GB、单次发送总量与单文件上限对齐 2GB、会话 12 小时、默认 7 天/20 次",
		Patch: json.RawMessage(`{
			"upload_ex": {"open_upload": false, "anonymous_daily_count": 0, "anonymous_daily_bytes": 0, "max_file_size": 2147483648},
			"user": {"allowuserregistration": false, "useruploadsize": 2147483648, "userstoragequota": 10737418240, "sessionexpiryhours": 12},
			"download": {"require_login": false},
			"transfer": {"max_count": 20, "expire_default": 7}
		}`),
	},
}

// PolicyTemplates 内置策略模板清单（副本，调用方改动不影响真相源）。
// 经 GET /admin/config 的 policy_templates 顶层键下发；导出 = 客户端序列化本清单
// （内置模板随代码版本走，自定义模板的导入/随备份包为后续拆分）。
func PolicyTemplates() []PolicyTemplate {
	out := make([]PolicyTemplate, len(builtinPolicyTemplates))
	copy(out, builtinPolicyTemplates)
	return out
}

// LookupPolicyTemplate 按 ID 查模板。
func LookupPolicyTemplate(id string) (PolicyTemplate, bool) {
	for _, t := range builtinPolicyTemplates {
		if t.ID == id {
			return t, true
		}
	}
	return PolicyTemplate{}, false
}

// ValidateConfigInvariants 跨配置不变量（保存期校验，违反即拒绝）。
//
// 目前收录的不变量（与管理台告警/对齐按钮同一判据，见前端 utils/uploadCap.ts）：
//
//	① 单次发送总量 ≥ 单文件上限（两者均 >0 时）——否则「超过单文件上限的文件
//	   永远发不出去」：先被总量闸拒，而 UI 仍在宣称单文件上限有效
//	   （2026-10-30 出厂默认自相矛盾事故即此类）。
//
// 0 语义为「不限」，不构成矛盾（与前端 uploadCapMismatch 同口径）。
// 新增不变量往这里加，并同步前端告警判据。
func ValidateConfigInvariants(cfg *SystemConfig) error {
	if cfg == nil {
		return nil
	}
	// 取不到的段回退生效值（nil = 未提交/未设置 → 运行时以 yaml/conf 为准）
	var total, perFile int64
	if cfg.User != nil {
		total = cfg.User.UserUploadSize
	} else if u := userSettingsFromYAML(); u != nil {
		total = u.UserUploadSize
	}
	if cfg.UploadEx != nil {
		perFile = cfg.UploadEx.MaxFileSize
	} else if g := conf.Snapshot(); g != nil {
		perFile = g.Upload.MaxFileSize
	}
	if gate.BatchCapBelowFileCap(total, perFile) {
		return fmt.Errorf(
			"%w: 单次发送总量上限（%d 字节）低于单文件上限（%d 字节）——超过单文件上限的文件永远发不出去；请先把总量上限抬到 ≥ 单文件上限（管理台「上传与分享」分区有对齐按钮）",
			ErrPolicyInvalid, total, perFile)
	}
	return nil
}

// ApplyPolicyTemplate 应用策略模板：模板 patch 字段级合并到已落库配置，
// 校验通过后走既有 UpdateConfig 通道（写库 system_configs → overlay 全局 conf →
// OnOverlayApplied → env 恒覆盖重施 → 多副本广播），一次下发多配置项。
//
// 错误口径：
//   - 未知模板 / 模板组合违反不变量 / 合并结果非法 → ErrPolicyInvalid（400）
//   - DB/装载失败 → 原样上抛（500 脱敏）
//
// 幂等：同一模板重复应用得到相同配置（第二次合并结果与现状一致，无副作用差异）。
func (s *Service) ApplyPolicyTemplate(ctx context.Context, id string) error {
	tpl, ok := LookupPolicyTemplate(id)
	if !ok {
		return fmt.Errorf("%w: 未知策略模板 %q（可用: %s）", ErrPolicyInvalid, id, policyTemplateIDs())
	}
	var patch map[string]json.RawMessage
	if err := json.Unmarshal(tpl.Patch, &patch); err != nil {
		return fmt.Errorf("%w: 模板 %s 的 patch 不是 JSON 对象: %v", ErrPolicyInvalid, tpl.ID, err)
	}

	// 底座 = 已落库配置（与 UpdateConfigJSON 同口径：不把 env/运行态覆盖值当底座）
	persisted, err := s.loadPersistedConfig(ctx)
	if err != nil {
		return err // 记录损坏/DB 故障：显式失败，不用默认值顶班（core#137 同族）
	}
	base := map[string]json.RawMessage{}
	if persisted != nil {
		b, mErr := json.Marshal(persisted)
		if mErr != nil {
			return mErr
		}
		if mErr = json.Unmarshal(b, &base); mErr != nil {
			return mErr
		}
	}
	// 缺段补种当前生效值：字段级合并遇到「底座没有这个段」时，patch 会变成
	// 整段替换 ⇒ 模板未提及的字段被清零（2026-10-23 段内清零坑的「缺段」变体）。
	seedSectionsFromEffective(base, patch)

	merged, err := json.Marshal(mergeJSONObjects(base, patch))
	if err != nil {
		return err
	}
	cfg := &SystemConfig{}
	if err := json.Unmarshal(merged, cfg); err != nil {
		return fmt.Errorf("%w: 模板合并结果无法解析: %v", ErrPolicyInvalid, err)
	}
	// 保存前校验：先通用合法性，再跨配置不变量——非法组合在此被拒绝，
	// 不落库、不进运行期（hub#368 验收：错误模板组合在保存时即被拒绝）。
	if err := validateSystemConfig(cfg); err != nil {
		return fmt.Errorf("%w: %v", ErrPolicyInvalid, err)
	}
	if err := ValidateConfigInvariants(cfg); err != nil {
		return err // 已包 ErrPolicyInvalid
	}
	// 走既有通道：nil 保留合并 / 掩码回填 / 校验 / 落库 / overlay / 钩子 / 审计
	// 全部复用，本层不重复实现任何副作用。审计细节记模板 ID（core#219）。
	ctx = withAuditDetail(ctx, "policy_template="+tpl.ID)
	return s.UpdateConfig(ctx, cfg)
}

// policyTemplateIDs 拒绝文案里列出可用模板 ID（可操作文案）。
func policyTemplateIDs() string {
	ids := make([]string, 0, len(builtinPolicyTemplates))
	for _, t := range builtinPolicyTemplates {
		ids = append(ids, t.ID)
	}
	return strings.Join(ids, ", ")
}

// seedSectionsFromEffective 把「模板要改、但底座缺失」的段用当前生效值补种。
//
// 为什么必须补：mergeJSONObjects 对「底座没有的键」是整体替换（无物可递归），
// 于是 patch 里该段的子集会成为整段——模板未提及的字段全部清零。底座缺失只发生在
// 「该段从未在线保存过」的站点（如全新部署第一次应用模板）。
//
// 取值来源 = 全局 conf 当前生效值（yaml + env 重施后的运行态）：这些字段本就
// 没有「管理员提交意图」可保留（没存过），生效值就是最接近的真相；env 字段被
// 固化进库也无运行态影响（OnOverlayApplied 每次 overlay 后仍会把 env 施回去）。
//
// ⚠️ 模板新增改动段时必须在这里补对应 case——否则该段在全新站点上会被清零。
// user 段不需要：loadPersistedConfig 恒回填（无记录时用 yaml 默认），
// 底座永远有 "user" 键。
func seedSectionsFromEffective(base, patch map[string]json.RawMessage) {
	g := conf.Snapshot()
	if g == nil {
		return
	}
	for key := range patch {
		if _, ok := base[key]; ok {
			continue
		}
		var v any
		switch key {
		case "upload_ex":
			v = g.Upload
		case "download":
			v = g.Download
		case "transfer":
			v = TransferSettings{MaxCount: g.Transfer.MaxCount, ExpireDefault: g.Transfer.ExpireDefault}
		case "ui":
			v = g.UI
		case "notify":
			v = g.Notify
		case "local_import":
			v = g.Upload.LocalImport
		case "oidc":
			v = g.Security.OIDC
		case "api_token":
			v = g.Security.APIToken
		default:
			continue // 未登记的段不补种（保持既有行为），给模板作者显式扩展点
		}
		if b, err := json.Marshal(v); err == nil {
			base[key] = b
		}
	}
}
