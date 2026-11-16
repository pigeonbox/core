// 管理写路由 × 审计覆盖守卫（core#219）。
//
// ## 要解决的结构性缺口
//
// pkg/audit 的写入站点守卫（audit_entry_guard_test.go）只保证「已登记的站点
// 仍在调执行点」，不保证「新管理写路由必须登记」——新增一条 POST/PUT/DELETE
// 管理端点、忘了审计，既有守卫全绿。
//
// ## 判据
//
//	① 扫描真实注册集（与路由策略矩阵同一推导：buildRoutePolicies），取
//	   auth=admin 且 path 前缀 /admin 的 POST/PUT/DELETE/PATCH 路由；
//	② 每条路由必须二选一：
//	     · 登记在 adminWriteAuditSites（文件 + 审计调用标记，标记必须真实存在）；
//	     · 登记在 adminWriteAuditExemptions（理由非空：为什么不需要审计）；
//	③ 反向对账：登记/豁免条目已不在路由表 ⇒ 陈旧红（路由改名/删除后忘同步）；
//	④ 敏感 GET（备份下载 = 全量 DB+对象）单独登记审计标记。
//
// 新增管理写路由时本用例必红，直到你补审计或写明豁免理由。
package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// adminWriteAuditSite 一条管理写路由的审计落点。
type adminWriteAuditSite struct {
	// File 承载审计调用的源文件（相对仓根）。
	File string
	// Marker 该文件必须包含的审计调用标记（action 字符串或调用形态）。
	Marker string
}

// adminWriteAuditSites 已审计的管理写路由（core#219 覆盖面）。
var adminWriteAuditSites = map[string]adminWriteAuditSite{
	// ---- 备份（全量 DB/对象，不可逆外泄面）----
	"POST /admin/backups/run": {File: "bootstrap/auto_backup.go", Marker: `audit.Record(ctx, "backup.run"`},

	// ---- 系统配置（可关闭匿名/审核/限流等安全开关）----
	"PUT /admin/config":      {File: "app/config/service.go", Marker: `s.logConfigOperation(ctx, "config.update"`},
	"PUT /admin/config/user": {File: "app/config/service.go", Marker: `s.logConfigOperation(ctx, "config.update"`},

	// ---- 文件治理 ----
	"DELETE /admin/files/:id":        {File: "app/admin/files.go", Marker: `"file.delete"`},
	"PUT /admin/files/:id":           {File: "gen/handler/admin/admin_manage.go", Marker: `"file.update"`},
	"PUT /admin/files/:id/status":    {File: "gen/handler/admin/admin_manage.go", Marker: `"file."+req.Status`},
	"POST /admin/files/batch-delete": {File: "gen/handler/admin/admin_manage.go", Marker: `"file.batch_delete"`},
	"POST /admin/files/batch-extend": {File: "gen/handler/admin/admin_manage.go", Marker: `"file.batch_extend"`},
	"POST /admin/files/batch-status": {File: "gen/handler/admin/admin_manage.go", Marker: `"file.batch-"+req.Status`},
	"POST /admin/files/purge":        {File: "gen/handler/admin/admin_manage.go", Marker: `"file.purge"`},
	"POST /admin/files/restore":      {File: "gen/handler/admin/admin_manage.go", Marker: `"file.restore"`},

	// ---- 本地文件管理（删服务器任意白名单文件 / 把本机文件变成分享）----
	"DELETE /admin/local-files":      {File: "gen/handler/admin/localfiles_ops.go", Marker: `"localfile.delete"`},
	"POST /admin/local-files/import": {File: "gen/handler/admin/localfiles_ops.go", Marker: `"localfile.import"`},

	// ---- 维护 ----
	"POST /admin/maintenance/clean-expired": {File: "app/admin/maintenance.go", Marker: `"maintenance.clean_expired_files"`},
	"POST /admin/maintenance/clean-temp":    {File: "app/admin/maintenance.go", Marker: `"maintenance.clean_incomplete_uploads"`},

	// ---- 联邦邀请 ----
	"POST /admin/p2p/invite-push":    {File: "gen/handler/p2p/admin_p2_pservice.go", Marker: `"p2p.invite.push"`},
	"POST /admin/p2p/invites/accept": {File: "gen/handler/p2p/admin_p2_pservice.go", Marker: `"p2p.invite.accept"`},

	// ---- 限流配置（关掉限流 = 打开爆破面）----
	"PUT /admin/ratelimit/config": {File: "gen/handler/ratelimit/ratelimit_service.go", Marker: `"ratelimit.config.update"`},

	// ---- 用户管理 ----
	"POST /admin/users":                    {File: "gen/handler/admin/admin_manage.go", Marker: `"user.create"`},
	"DELETE /admin/users/:id":              {File: "app/admin/users.go", Marker: `"user.delete"`},
	"PUT /admin/users/:id":                 {File: "gen/handler/admin/admin_manage.go", Marker: `"user.update"`},
	"POST /admin/users/:id/reset-password": {File: "gen/handler/admin/admin_manage.go", Marker: `"user.reset_password"`},
	"PUT /admin/users/:id/status":          {File: "app/admin/users.go", Marker: `"user.update_status"`},

	// ---- 管理员 Passkey（长期免口令凭据）----
	"POST /admin/webauthn/register/verify":   {File: "bootstrap/admin_webauthn.go", Marker: `"passkey.register"`},
	"DELETE /admin/webauthn/credentials/:id": {File: "bootstrap/admin_webauthn.go", Marker: `"passkey.delete"`},

	// ---- 存储切换/迁移/配置：经 activate → SaveRuntimeStorage 的持久化审计 ----
	"POST /admin/storage/switch":  {File: "app/config/service.go", Marker: `"storage.config.persist"`},
	"PUT /admin/storage/config":   {File: "app/config/service.go", Marker: `"storage.config.persist"`},
	"POST /admin/storage/migrate": {File: "app/config/service.go", Marker: `"storage.config.persist"`},
}

// adminWriteAuditExemptions 不需要审计的管理写路由 + 理由（必须写明为什么）。
var adminWriteAuditExemptions = map[string]string{
	"POST /admin/login": "认证事件：登录成功/失败由登录链路与限流记录，不含管理数据变更",

	"POST /admin/maintenance/optimize": "VACUUM/ANALYZE 可重放维护动作，无数据丢失语义",

	"POST /admin/notifies":       "站内公告内容运营（非安全开关/数据销毁），后续批次",
	"PUT /admin/notifies/:id":    "站内公告内容运营（非安全开关/数据销毁），后续批次",
	"DELETE /admin/notifies/:id": "站内公告内容运营（非安全开关/数据销毁），后续批次",

	"POST /admin/notify/channels/test": "探针/测试发送，无持久化变更",
	"POST /admin/notify/smtp/test":     "探针/测试发送，无持久化变更",
	"POST /admin/oidc/test":            "探针/发现端点验证，无持久化变更",
	"POST /admin/ratelimit/test":       "探针/限流器实测，无配置变更",

	"POST /admin/webauthn/register/options": "登记第一步（挑战），不产生凭据；verify 已审计",

	"POST /admin/storage/clean-presign-orphans": "只清理 24h 宽限期的未完成直传孤儿对象，不动已登记分享数据",
	"POST /admin/storage/gc-jobs/replay":        "GC 死信重放（重放已存在的删除任务），无新数据销毁语义",
}

// sensitiveAdminReads 读路径但属高危外泄面的端点（core#219 点名备份下载）。
var sensitiveAdminReads = map[string]adminWriteAuditSite{
	"GET /admin/backups/:name": {File: "bootstrap/auto_backup.go", Marker: `audit.Record(ctx, "backup.download"`},
}

// TestAdminWriteRoutesAudited ①-③：管理写路由必须登记审计或豁免。
func TestAdminWriteRoutesAudited(t *testing.T) {
	policies := buildRoutePolicies(t)

	all := map[string]bool{}
	var adminWrites []string
	for _, p := range policies {
		all[p.key()] = true
		if p.Auth != "admin" || !strings.HasPrefix(p.Path, "/admin/") {
			continue
		}
		switch p.Method {
		case "POST", "PUT", "DELETE", "PATCH":
			adminWrites = append(adminWrites, p.key())
		}
	}
	if len(adminWrites) == 0 {
		t.Fatal("未找到任何管理写路由——路由推导失效（守卫自身失效）")
	}

	seen := map[string]bool{}
	for _, key := range adminWrites {
		seen[key] = true
		site, audited := adminWriteAuditSites[key]
		reason, exempt := adminWriteAuditExemptions[key]
		switch {
		case audited && exempt:
			t.Errorf("管理写路由 %s 同时登记了审计与豁免——二选一", key)
		case audited:
			assertAuditMarker(t, key, site)
		case exempt:
			if strings.TrimSpace(reason) == "" {
				t.Errorf("管理写路由 %s 的豁免理由为空——豁免必须写明为什么不需要审计", key)
			}
		default:
			t.Errorf("管理写路由 %s 未登记审计：补审计调用并登记 adminWriteAuditSites，"+
				"或在 adminWriteAuditExemptions 写明豁免理由（core#219）", key)
		}
	}

	// 反向对账：路由已删除/改名 ⇒ 登记陈旧。
	for key := range adminWriteAuditSites {
		if !all[key] {
			t.Errorf("审计登记陈旧：%s 已不在路由表（路由改名/删除后忘记同步）", key)
		}
	}
	for key := range adminWriteAuditExemptions {
		if !all[key] {
			t.Errorf("审计豁免陈旧：%s 已不在路由表（路由改名/删除后忘记同步）", key)
		}
	}
}

// TestSensitiveAdminReadsAudited ④：敏感读端点（备份下载）也必须有审计。
func TestSensitiveAdminReadsAudited(t *testing.T) {
	policies := buildRoutePolicies(t)
	all := map[string]bool{}
	for _, p := range policies {
		all[p.key()] = true
	}
	for key, site := range sensitiveAdminReads {
		if !all[key] {
			t.Errorf("敏感读登记陈旧：%s 已不在路由表", key)
			continue
		}
		assertAuditMarker(t, key, site)
	}
}

// assertAuditMarker 登记文件必须真实包含审计调用标记。
func assertAuditMarker(t *testing.T, route string, site adminWriteAuditSite) {
	t.Helper()
	if site.File == "" || site.Marker == "" {
		t.Errorf("路由 %s 的审计登记不完整（File/Marker 必填）", route)
		return
	}
	content, err := os.ReadFile(filepath.Join("..", filepath.FromSlash(site.File)))
	if err != nil {
		t.Errorf("路由 %s 的审计承载文件 %s 读取失败: %v", route, site.File, err)
		return
	}
	if !strings.Contains(string(content), site.Marker) {
		t.Errorf("路由 %s 登记了审计，但 %s 找不到标记 %q（调用被删/改）",
			route, site.File, site.Marker)
	}
}
