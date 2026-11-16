// core#219：配置写入审计记录**字段级键名**（不落值）。
package config

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/pkg/audit"
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

// 保障细节摘要不把口令写进审计：配置体含 SMTP 口令时，target 只能有键名。
func TestConfigAuditNeverContainsSecretValues(t *testing.T) {
	var incoming map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(`{"notify":{"smtp":{"password":"super-secret"}}}`), &incoming))
	detail := auditKeysDetail("keys=", configChangeKeys(incoming))
	assert.Contains(t, detail, "notify.smtp.password")
	assert.NotContains(t, detail, "super-secret")
}
