// Package policy 路由策略声明矩阵（core#173：统一 Policy Enforcement）。
//
// 安全规则此前散落在各 handler/中间件里，新增公开入口没有强制申报机制，
// 绕过（core#163/core#164 类）只能靠事后审计发现。本包把「鉴权级别 × 频控桶 ×
// 归属」收敛为一张**声明表**（declarations.json，人工复核产物），以
// gen/router 生成物为真相源做静态扫描：
//
//   - gen/router 里出现的每一条路由，必须在声明表中存在 → 缺声明 ⇒ CI 红；
//   - 声明表中每一条，必须仍能在 gen/router 里找到 → 防声明腐烂；
//   - 声明的鉴权级别必须与静态推导（Group 祖先链中间件）一致，
//     显式 override 例外（人工复核标记，如 /admin/login 白名单）。
//
// 六策略模块与本仓代码的落点（逻辑分层，不新增服务）：
//
//	Authentication → pkg/middleware（AdminMiddleware/Auth/ApiKey/OptionalIdentity）
//	Authorization  → middleware scope 检查 + apikey scope（apikey_scope_test）
//	Pickup Policy  → pkg/gate/pickup（取件码枚举锁定/分享密码/取件次数；#163 修复）
//	Egress Policy  → pkg/outbound（SSRF dial/redirect 守卫；#164 修复落点）
//	Resource Policy→ pkg/gate（upload 闸门）+ app/share/quota_reserve（配额预留账本）
//	Audit          → pkg/middleware/auditactor + pkg/audit
package policy

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
)

//go:embed declarations.json
var declarationsFS embed.FS

// AuthLevel 鉴权级别（与静态推导同词表）。
type AuthLevel string

const (
	AuthAnonymous AuthLevel = "anonymous"
	AuthOptional  AuthLevel = "optional"
	AuthUser      AuthLevel = "user"
	AuthUserOrKey AuthLevel = "user_or_apikey"
	AuthAdmin     AuthLevel = "admin"
)

// Policy 单条路由策略声明。
type Policy struct {
	Pkg       string `json:"pkg"`
	Handler   string `json:"handler"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Auth      string `json:"auth"`
	Bucket    string `json:"bucket"`
	Ownership string `json:"ownership"`
	// Override 非空 = 人工复核例外（如 /admin/login 白名单）；
	// 校验器跳过"声明 auth vs 静态链 auth"一致性，但**仍要求声明存在**。
	Override string `json:"override,omitempty"`
}

// Route 扫描器产出的路由清单条目。
type Route struct {
	Pkg     string
	Handler string
	Method  string
	Path    string
	// ChainAuth 静态推导的鉴权级别（Group 祖先链中间件）。
	ChainAuth string
}

// LoadDeclarations 从嵌入的 declarations.json 读取声明表。
func LoadDeclarations() ([]Policy, error) {
	raw, err := declarationsFS.ReadFile("declarations.json")
	if err != nil {
		return nil, fmt.Errorf("policy: 读取声明表: %w", err)
	}
	var doc struct {
		Routes []Policy `json:"routes"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("policy: 解析声明表: %w", err)
	}
	return doc.Routes, nil
}

// authRank 鉴权强度序：admin > user_or_apikey ≈ user > optional > anonymous。
// 声明可以比静态推导**更严**（如静态 optional、声明 user——handler 内部再收紧），
// 不允许比静态推导**更松**（声明 anonymous 但链上有 AuthMiddleware = 声明了假策略）。
var authRank = map[string]int{
	string(AuthAnonymous): 0, string(AuthOptional): 1,
	string(AuthUserOrKey): 2, string(AuthUser): 2,
	string(AuthAdmin): 3,
}

// Check 对比路由清单与声明表，返回全部违例（空 = 通过）。
// 负对照即本函数的单元测试：删一条声明必产生 violation。
func Check(inventory []Route, decls []Policy) []string {
	declKey := func(p Policy) string { return p.Method + " " + p.Pkg + "/" + p.Handler + " " + p.Path }
	routeKey := func(r Route) string { return r.Method + " " + r.Pkg + "/" + r.Handler + " " + r.Path }

	declByKey := make(map[string]Policy, len(decls))
	for _, d := range decls {
		declByKey[declKey(d)] = d
	}
	routeByKey := make(map[string]Route, len(inventory))
	for _, r := range inventory {
		routeByKey[routeKey(r)] = r
	}

	var violations []string
	// 1) 每条真实路由必须有声明
	for _, r := range inventory {
		k := routeKey(r)
		d, ok := declByKey[k]
		if !ok {
			violations = append(violations, fmt.Sprintf(
				"MISSING-DECLARATION: 路由 %s 未在声明表登记（新增公开入口必须申报 鉴权×频控×归属；修法：declarations.json 增加 entries）", k))
			continue
		}
		// 2) 鉴权一致性（override 条目跳过——人工复核标记）
		if d.Override == "" && d.Auth != r.ChainAuth {
			lo, okLo := authRank[d.Auth]
			hi, okHi := authRank[r.ChainAuth]
			if !okLo || !okHi || lo < hi {
				violations = append(violations, fmt.Sprintf(
					"AUTH-MISMATCH: %s 声明=%s 静态链=%s（声明不得比静态链更松；确属中间件白名单例外请加 override 字段）", k, d.Auth, r.ChainAuth))
			}
		}
	}
	// 3) 声明不得腐烂（路由已删但声明还在）
	routeKeys := make(map[string]bool, len(inventory))
	for k := range routeByKey {
		routeKeys[k] = true
	}
	for _, d := range decls {
		k := declKey(d)
		if !routeKeys[k] {
			violations = append(violations, fmt.Sprintf(
				"STALE-DECLARATION: 声明 %s 在 gen/router 已不存在（路由已删请同步删声明）", k))
		}
	}
	sort.Strings(violations)
	return violations
}
