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
	"unicode/utf8"

	"github.com/pigeonbox/core/repo/db/model"
)

// maxAuditKeyPaths 单条审计 target 里最多列出的字段键数。
const maxAuditKeyPaths = 50

// auditTargetBase 配置写入审计的固定文案（target 的前缀）。
const auditTargetBase = "system config persisted to database"

// maxAuditTargetLen 审计 target 的字符上限 = 审计表的列宽
// （model.AdminOperationLogTargetMaxLen）。
//
// 为什么 target 有硬上限：target 是**人拼的字符串**（键名清单，长度随本次改动
// 字段数增长）⇒ 列宽是硬约束：PG/MySQL 严格模式超长写不进；换库导入按
// 「数据最长 vs 模型声明」逐列预检 ⇒ 超长即拒导（2026-11-17 实录：50 个键名
// 拼出 555 字符 > 255 ⇒ 换库导入红；SQLite 不校验 varchar 长度 ⇒ 只有换库才炸）。
//
// ⇒ 口径：**在源头按列宽预算选键**——装不下的键不逐个截断（会切出半个键名），
// 而是少列几个，末尾用「…(+N)」标注还剩多少个 ⇒ 既不超长，也不丢
// 「本次改了很多开关」这一事实。
const maxAuditTargetLen = model.AdminOperationLogTargetMaxLen

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
	if d := auditDetailOf(ctx); d != "" {
		return auditTargetBase + " (" + d + ")"
	}
	return auditTargetBase
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
//
// 长度预算 = 审计 target 的列宽（maxAuditTargetLen）减去固定文案与括号、再减去
// 本摘要的前缀 ⇒ 摘要装得下 ⇒ 整条 target 装得下。
//
// 装不下的键**不逐个截断**（会切出半个键名，事后无法判断改的是哪个开关），
// 而是少列几个，末尾用「…(+N)」标注还剩多少个 ⇒ 既不超长，也不丢
// 「本次改了很多开关」这一事实。
func auditKeysDetail(prefix string, keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	budget := maxAuditTargetLen - len(auditTargetBase) - len(" ()") - len(prefix)
	if budget <= 0 {
		return truncateAuditText(prefix+omittedAuditKeys(len(keys)), budget)
	}
	var b strings.Builder
	b.WriteString(prefix)
	used := 0
	kept := 0
	for _, k := range keys {
		seg := k
		if kept > 0 {
			seg = "," + k
		}
		// 装下这个键之后，剩下的键数决定「…(+N)」标注的长度 ⇒ 先把它预留出来。
		reserve := len(omittedAuditKeys(len(keys) - kept - 1))
		if used+len(seg)+reserve > budget {
			break
		}
		b.WriteString(seg)
		used += len(seg)
		kept++
	}
	if kept < len(keys) {
		b.WriteString(omittedAuditKeys(len(keys) - kept))
	}
	return truncateAuditText(b.String(), budget)
}

// omittedAuditKeys 被省略键数的标注（…(+N)）。
func omittedAuditKeys(n int) string {
	return fmt.Sprintf("…(+%d)", n)
}

// truncateAuditText 按字节上限截断（不切碎多字节字符）。
//
// 列宽判据用的是**字节**长度（换库导入的列宽预检同口径）⇒ 截断也按字节；
// 但切点落在多字节字符中间会产生非法 UTF-8 ⇒ 回退到字符边界。
func truncateAuditText(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
