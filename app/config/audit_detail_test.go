// core#219：配置写入审计记录**字段级键名**（不落值）。
package config

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/pkg/audit"
	"github.com/pigeonbox/core/repo/db/model"
)

type auditCaptureSink struct{ entries []audit.Entry }

func (s *auditCaptureSink) Create(_ context.Context, e audit.Entry) error {
	s.entries = append(s.entries, e)
	return nil
}

func useAuditCapture(t *testing.T) *auditCaptureSink {
	t.Helper()
	s := &auditCaptureSink{}
	audit.SetSink(s)
	t.Cleanup(func() { audit.SetSink(nil) })
	return s
}

func TestConfigChangeKeys(t *testing.T) {
	var incoming map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(`{
		"base":{"name":"x","public_url":"https://e"},
		"upload_ex":{"allowed_expire_styles":["day","count"]},
		"notify":{"smtp":{"host":"smtp.example.com","tls_mode":"required"}},
		"local_import":null
	}`), &incoming))

	keys := configChangeKeys(incoming)
	assert.ElementsMatch(t, []string{
		"base.name", "base.public_url",
		"upload_ex.allowed_expire_styles",
		"notify.smtp.host", "notify.smtp.tls_mode",
		"local_import",
	}, keys)
	for _, k := range keys {
		assert.NotContains(t, k, "smtp.example.com", "键路径不得包含值")
	}
}

func TestUpdateConfigJSON_AuditRecordsKeysOnly(t *testing.T) {
	sink := useAuditCapture(t)
	svc := newTestConfigService(t)
	ctx := context.Background()

	require.NoError(t, svc.UpdateConfigJSON(ctx, []byte(
		`{"base":{"name":"站点"},"notify":{"smtp":{"host":"smtp.example.com","tls_mode":"required"}}}`)))

	require.NotEmpty(t, sink.entries)
	var found bool
	for _, e := range sink.entries {
		if e.Action != "config.update" {
			continue
		}
		found = true
		assert.True(t, e.Success)
		assert.Contains(t, e.Target, "base.name")
		assert.Contains(t, e.Target, "notify.smtp.host")
		assert.Contains(t, e.Target, "notify.smtp.tls_mode")
		assert.NotContains(t, e.Target, "smtp.example.com", "审计不得落配置值（可能含凭据）")
	}
	assert.True(t, found, "配置写入必须产生 config.update 审计行：%+v", sink.entries)
}

func TestUpdateUserSettingsJSON_AuditRecordsUserKeys(t *testing.T) {
	sink := useAuditCapture(t)
	svc := newTestConfigService(t)
	ctx := context.Background()

	require.NoError(t, svc.UpdateUserSettingsJSON(ctx, []byte(`{"allowuserregistration":true}`)))
	require.NotEmpty(t, sink.entries)
	var found bool
	for _, e := range sink.entries {
		if e.Action != "config.update" {
			continue
		}
		found = true
		assert.Contains(t, e.Target, "user.keys=allowuserregistration")
		assert.NotContains(t, e.Target, "true", "审计不得落配置值")
	}
	assert.True(t, found, "%+v", sink.entries)
}

func TestApplyPolicyTemplate_AuditRecordsTemplateID(t *testing.T) {
	sink := useAuditCapture(t)
	svc := newTestConfigService(t)
	ctx := context.Background()

	require.NoError(t, svc.ApplyPolicyTemplate(ctx, "intranet"))
	require.NotEmpty(t, sink.entries)
	var found bool
	for _, e := range sink.entries {
		if e.Action != "config.update" {
			continue
		}
		found = true
		assert.Contains(t, e.Target, "policy_template=intranet")
	}
	assert.True(t, found, "%+v", sink.entries)
}

// 审计 target 不得超过审计表的列宽（admin_operation_logs.target 的 size）。
//
// 2026-11-17 实录：配置写入审计拼「本次改动的键名清单」，50 个键名 ≈ 555 字符
// > 列宽 255 ⇒ SQLite 写得进（不校验 varchar 长度）⇒ 生产无感；**换库导入**
// 按「数据最长 vs 模型声明」逐列预检 ⇒ 拒导（S26b 换库导入红）。
func TestConfigAuditTargetNeverExceedsColumnWidth(t *testing.T) {
	// 60 个长键名（够长 ⇒ 必然撑破列宽）
	raw := make(map[string]json.RawMessage, 60)
	for i := 0; i < 60; i++ {
		raw[fmt.Sprintf("section_%02d.very_long_option_name_%02d", i, i)] = json.RawMessage(`true`)
	}
	incoming := raw

	keys := configChangeKeys(incoming)
	detail := auditKeysDetail("keys=", keys)
	target := configAuditTarget(withAuditDetail(context.Background(), detail))

	assert.LessOrEqual(t, len(target), model.AdminOperationLogTargetMaxLen,
		"审计 target 超列宽（写入方按常量封顶的承诺被打破）")
	assert.Contains(t, detail, "…(+", "装不下的键必须标注省略数量（不能静默丢）")
	assert.Contains(t, target, "system config persisted to database")
}

// 负对照：摘要装得下时**不得**出现省略标注（不许把短摘要也截了）。
func TestConfigAuditDetailKeepsShortListIntact(t *testing.T) {
	var incoming map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(`{"base":{"name":"x"},"notify":{"smtp":{"host":"h"}}}`), &incoming))
	detail := auditKeysDetail("keys=", configChangeKeys(incoming))
	assert.NotContains(t, detail, "…(+")
	assert.Contains(t, detail, "base.name")
	assert.Contains(t, detail, "notify.smtp.host")
}

// 保障细节摘要不把口令写进审计：配置体含 SMTP 口令时，target 只能有键名。
func TestConfigAuditNeverContainsSecretValues(t *testing.T) {
	var incoming map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(`{"notify":{"smtp":{"password":"super-secret"}}}`), &incoming))
	detail := auditKeysDetail("keys=", configChangeKeys(incoming))
	assert.Contains(t, detail, "notify.smtp.password")
	assert.NotContains(t, detail, "super-secret")
}
