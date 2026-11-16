// audit_detail.go 配置写入的审计细节：记录**字段级键名**，不落值（core#219）。
//
// 背景：PUT /admin/config 的审计行此前只有固定文案「system config persisted
// to database」——管理员到底改了哪个开关（匿名上传/审核/限流…）事后无法回答。
// 但配置体里可能含 SMTP 口令/OIDC secret 等凭据 ⇒ 审计只能记**键路径**，
// 绝不能把值写进 admin_operation_logs（pkg/audit 的脱敏约束）。
//
// 实现：提交体（字段级 patch）的叶子路径经 context 传给 UpdateConfig 的审计
// 调用点（单一行、单一次落盘）。嵌套对象递归；数组/标量/null 视为叶子。
// 键数上限 50，超出以「…(+N)」标注，避免把超长 target 写进审计表。
package config

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// maxAuditKeyPaths 单条审计 target 里最多列出的字段键数。
const maxAuditKeyPaths = 50

type auditDetailCtxKey struct{}

// withAuditDetail 把「本次配置写入的键名摘要」附到 ctx（仅本包内部使用）。
func withAuditDetail(ctx context.Context, detail string) context.Context {
	if detail == "" {
		return ctx
	}
	return context.WithValue(ctx, auditDetailCtxKey{}, detail)
}

// auditDetailOf 取 ctx 上的键名摘要（无则空）。
func auditDetailOf(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(auditDetailCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// configAuditTarget 配置写入的审计 target：固定文案 + 本次改动的键名摘要。
func configAuditTarget(ctx context.Context) string {
	const base = "system config persisted to database"
	if d := auditDetailOf(ctx); d != "" {
		return base + " (" + d + ")"
	}
	return base
}

// configChangeKeys 从字段级提交体抽取排序后的叶子键路径（不含值）。
func configChangeKeys(incoming map[string]json.RawMessage) []string {
	out := make([]string, 0, len(incoming))
	for k, v := range incoming {
		collectJSONKeyPaths(k, v, &out)
	}
	sort.Strings(out)
	// 去重（理论上不会重复，防御重复键解析的边角）
	uniq := out[:0]
	for i, k := range out {
		if i == 0 || k != out[i-1] {
			uniq = append(uniq, k)
		}
	}
	out = uniq
	if len(out) > maxAuditKeyPaths {
		extra := len(out) - maxAuditKeyPaths
		out = append(out[:maxAuditKeyPaths:maxAuditKeyPaths], fmt.Sprintf("…(+%d)", extra))
	}
	return out
}

// collectJSONKeyPaths 递归收集叶子路径：对象继续下钻，其余（标量/数组/null）
// 以当前路径为叶子。空对象不产生路径（没有具体字段被改动）。
func collectJSONKeyPaths(prefix string, raw json.RawMessage, out *[]string) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err == nil && m != nil {
		if len(m) == 0 {
			return
		}
		for k, v := range m {
			collectJSONKeyPaths(prefix+"."+k, v, out)
		}
		return
	}
	*out = append(*out, prefix)
}

// auditKeysDetail 把键路径切片拼成 target 里的摘要（空切片返回空串）。
func auditKeysDetail(prefix string, keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	return prefix + strings.Join(keys, ",")
}
