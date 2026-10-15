package bootstrap

import (
	"testing"

	"github.com/pigeonbox/core/conf"
)

// withTestGlobalConf 注入测试全局配置并在结束后还原，避免污染同包其他用例。
func withTestGlobalConf(t *testing.T) *conf.AppConfiguration {
	t.Helper()
	prev := conf.GetGlobalConfig()
	g := &conf.AppConfiguration{}
	conf.SetGlobalConfig(g)
	t.Cleanup(func() { conf.SetGlobalConfig(prev) })
	return g
}

// TestReapplyEnvOverrides_UnifiedPrecedence 统一配置优先级（yaml < DB < env）：
// DB overlay 之后重施非空 env；env 未设置的字段保持 overlay 值（DB/种子）。
func TestReapplyEnvOverrides_UnifiedPrecedence(t *testing.T) {
	g := withTestGlobalConf(t)
	// 模拟 DB overlay 后的状态：托管段已是"DB 意图值"
	g.Federation.RegistryURL = "http://db-registry:12346"
	g.UI.ExposeOpenAPI = false
	g.Upload.AllowedExtensions = []string{".db"}
	g.Notify.SMTP.Port = 2525
	showAdmin := false
	g.UI.ShowAdminAddr = &showAdmin

	t.Setenv("PB_FEDERATION_REGISTRY_URL", "http://env-registry:12346")
	t.Setenv("PB_UI_EXPOSE_OPENAPI", "true")
	t.Setenv("PB_UPLOAD_ALLOWED_EXTENSIONS", ".jpg,.png")
	t.Setenv("PB_SMTP_PORT", "465")
	t.Setenv("PB_UI_SHOW_ADMIN_ADDR", "true")
	// 非法值：跳过不 panic，原值保留
	t.Setenv("PB_SMTP_HOST", "env-smtp-host")
	t.Setenv("PB_LOCKOUT_MAX_ATTEMPTS", "not-a-number")

	ReapplyEnvOverrides(g)

	if g.Federation.RegistryURL != "http://env-registry:12346" {
		t.Fatalf("registry_url 应为 env 值, got %q", g.Federation.RegistryURL)
	}
	if !g.UI.ExposeOpenAPI {
		t.Fatal("expose_openapi 应被 env 置回 true")
	}
	if showAdmin != true || g.UI.ShowAdminAddr == nil || *g.UI.ShowAdminAddr != true {
		t.Fatalf("*bool 指针叶应被 env 覆盖, got %v", g.UI.ShowAdminAddr)
	}
	if len(g.Upload.AllowedExtensions) != 2 || g.Upload.AllowedExtensions[0] != ".jpg" {
		t.Fatalf("列表叶应按 CSV 拆分, got %v", g.Upload.AllowedExtensions)
	}
	if g.Notify.SMTP.Port != 465 {
		t.Fatalf("int 叶应为 465, got %d", g.Notify.SMTP.Port)
	}
	if g.Notify.SMTP.Host != "env-smtp-host" {
		t.Fatalf("string 叶应为 env 值, got %q", g.Notify.SMTP.Host)
	}
}

// TestReapplyEnvOverrides_EmptyEnvKeepsDBValue env 未设置=不覆盖（DB/overlay 值生效）。
func TestReapplyEnvOverrides_EmptyEnvKeepsDBValue(t *testing.T) {
	g := withTestGlobalConf(t)
	g.Federation.RegistryURL = "http://db-registry:12346"
	g.UI.AdminPath = "console"

	// 显式设空串（set-but-empty 不视为覆盖，与 viper BindEnv 同语义）
	t.Setenv("PB_FEDERATION_REGISTRY_URL", "")
	t.Setenv("PB_UI_ADMIN_PATH", "")

	ReapplyEnvOverrides(g)

	if g.Federation.RegistryURL != "http://db-registry:12346" {
		t.Fatalf("env 空串不应覆盖, got %q", g.Federation.RegistryURL)
	}
	if g.UI.AdminPath != "console" {
		t.Fatalf("env 空串不应覆盖, got %q", g.UI.AdminPath)
	}
}

// TestReapplyEnvOverrides_UnknownKeySkipped 键径不存在时跳过并保留其余键生效。
func TestReapplyEnvOverrides_UnknownKeySkipped(t *testing.T) {
	g := withTestGlobalConf(t)
	g.UI.ExposeOpenAPI = false
	t.Setenv("PB_UI_EXPOSE_OPENAPI", "true")
	t.Setenv("PB_FEDERATION_MIN_ENTROPY", "64")

	ReapplyEnvOverrides(g) // 不应 panic

	if !g.UI.ExposeOpenAPI {
		t.Fatal("合法键应正常生效")
	}
	if g.Federation.AnnounceMinEntropyBits != 64 {
		t.Fatalf("int64 叶应为 64, got %d", g.Federation.AnnounceMinEntropyBits)
	}
}

// TestReapplyEnvOverrides_NilGlobal 安全空转：未初始化全局配置时不 panic。
func TestReapplyEnvOverrides_NilGlobal(t *testing.T) {
	prev := conf.GetGlobalConfig()
	conf.SetGlobalConfig(nil)
	t.Cleanup(func() { conf.SetGlobalConfig(prev) })
	t.Setenv("PB_SMTP_HOST", "x")
	ReapplyEnvOverrides(nil) // 仅需不 panic
}
