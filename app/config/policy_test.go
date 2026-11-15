package config

// policy_test.go 策略模板层验收（hub#368）：
//   - 模板应用幂等（同一模板连打两次，配置一致）
//   - 非法组合在保存时被拒绝（ErrPolicyInvalid，不落库）
//   - env 恒覆盖不被模板破坏（DB 存模板意图，生效值仍以 env 为准）
//   - 内置模板逐个自洽（干净站点上应用不得被自己的不变量拒掉）
//   - 缺段补种：全新站点首打模板不清零未提及字段

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/conf"
)

// withGlobalConfForPolicy 注入全局 conf（seed/overlay 落点），结束还原空壳。
// 与 p2p_test.withGlobalConf 同语义；copy-on-write 下断言一律读 conf.Snapshot()。
func withGlobalConfForPolicy(t *testing.T) {
	t.Helper()
	conf.SetGlobalConfig(&conf.AppConfiguration{})
	t.Cleanup(func() { conf.SetGlobalConfig(&conf.AppConfiguration{}) })
}

// TestPolicyTemplates_SelfConsistent 内置模板逐个自洽：在干净底座上应用
// 不得被自己的跨配置不变量拒掉（否则模板一打就被拒，等于废模板）。
func TestPolicyTemplates_SelfConsistent(t *testing.T) {
	for _, tpl := range PolicyTemplates() {
		t.Run(tpl.ID, func(t *testing.T) {
			require.NotEmpty(t, tpl.ID)
			require.NotEmpty(t, tpl.Name)
			require.NotEmpty(t, tpl.Description)
			var patch map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(tpl.Patch, &patch), "patch 必须是 JSON 对象")
			require.NotEmpty(t, patch, "模板不得是空 patch（空 = 什么都没下发）")

			cfg := &SystemConfig{}
			require.NoError(t, json.Unmarshal(tpl.Patch, cfg), "patch 必须能解进 SystemConfig")
			require.NoError(t, validateSystemConfig(cfg), "模板自身必须通过通用校验")
			require.NoError(t, ValidateConfigInvariants(cfg), "模板自身必须满足跨配置不变量")
		})
	}
}

// TestPolicyTemplates_ListIsCopy 清单是副本：调用方改动不污染真相源。
func TestPolicyTemplates_ListIsCopy(t *testing.T) {
	a := PolicyTemplates()
	require.NotEmpty(t, a)
	a[0].Name = "MUTATED"
	b := PolicyTemplates()
	require.NotEqual(t, "MUTATED", b[0].Name, "PolicyTemplates 必须返回副本")
}

// TestApplyPolicyTemplate_Idempotent 模板应用幂等：同一模板连打两次，
// 第二次不报错且配置与第一次一致（落库 JSON 逐字节相同）。
func TestApplyPolicyTemplate_Idempotent(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	withGlobalConfForPolicy(t)

	require.NoError(t, svc.ApplyPolicyTemplate(ctx, "intranet"))
	first, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	firstJSON, err := json.Marshal(first)
	require.NoError(t, err)

	require.NoError(t, svc.ApplyPolicyTemplate(ctx, "intranet"))
	second, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	secondJSON, err := json.Marshal(second)
	require.NoError(t, err)

	require.JSONEq(t, string(firstJSON), string(secondJSON), "重复应用同一模板必须幂等")
	require.True(t, first.UploadEx.OpenUpload, "intranet 模板应打开访客上传")
	require.False(t, first.User.AllowUserRegistration, "intranet 模板应关闭自助注册")
	require.Equal(t, 7, first.Transfer.ExpireDefault)
	require.Equal(t, 100, first.Transfer.MaxCount)
	require.False(t, first.Download.RequireLogin, "intranet 模板应放开下载登录")
}

// TestApplyPolicyTemplate_AllTemplatesApply 三个内置模板都能在干净站点上应用，
// 且应用后各旋钮与模板 patch 一致（切换模板后各旋钮一致生效的验收）。
func TestApplyPolicyTemplate_AllTemplatesApply(t *testing.T) {
	wantOpenUpload := map[string]bool{
		"intranet":            true,
		"public-no-anonymous": false,
		"enterprise":          false,
	}
	wantRegistration := map[string]bool{
		"intranet":            false,
		"public-no-anonymous": true,
		"enterprise":          false,
	}
	for _, tpl := range PolicyTemplates() {
		t.Run(tpl.ID, func(t *testing.T) {
			svc := newTestConfigService(t)
			ctx := context.Background()
			withGlobalConfForPolicy(t)

			require.NoError(t, svc.ApplyPolicyTemplate(ctx, tpl.ID))
			got, err := svc.GetConfig(ctx)
			require.NoError(t, err)
			require.Equal(t, wantOpenUpload[tpl.ID], got.UploadEx.OpenUpload, "open_upload")
			require.Equal(t, wantRegistration[tpl.ID], got.User.AllowUserRegistration, "allowuserregistration")
			require.Equal(t, int64(0), got.UploadEx.AnonymousDailyCount, "匿名日配额（次）")
			require.Equal(t, int64(0), got.UploadEx.AnonymousDailyBytes, "匿名日配额（字节）")
			require.False(t, got.Download.RequireLogin, "download.require_login")

			// 一致性校验后置断言：应用后的配置不得违反不变量
			require.NoError(t, ValidateConfigInvariants(got))
		})
	}
}

// TestApplyPolicyTemplate_RejectsInvalidCombination 非法组合在保存时被拒绝：
// 底座里预置「总量 < 单文件」矛盾，再打一个不修尺寸的模板 ⇒ ErrPolicyInvalid，
// 且配置纹丝不动（不落库）。
func TestApplyPolicyTemplate_RejectsInvalidCombination(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	withGlobalConfForPolicy(t)

	// 预置矛盾态：单文件 4GB、总量 50MB（2026-10-30 事故形态）
	require.NoError(t, svc.UpdateConfigJSON(ctx, []byte(
		`{"upload_ex":{"max_file_size":4294967296},"user":{"useruploadsize":52428800}}`)))
	before, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	beforeJSON, err := json.Marshal(before)
	require.NoError(t, err)

	// intranet 模板不碰尺寸 ⇒ 合并结果仍是矛盾态 ⇒ 必须拒绝
	err = svc.ApplyPolicyTemplate(ctx, "intranet")
	require.Error(t, err, "矛盾组合必须在保存时被拒绝")
	require.True(t, errors.Is(err, ErrPolicyInvalid), "必须是 ErrPolicyInvalid（handler 据此回 400），got: %v", err)
	require.Contains(t, err.Error(), "单次发送总量上限", "拒绝文案必须点名矛盾项（可操作文案）")

	after, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	afterJSON, err := json.Marshal(after)
	require.NoError(t, err)
	require.JSONEq(t, string(beforeJSON), string(afterJSON), "被拒绝的保存不得改动配置")

	// 企业模板自带对齐（总量 = 单文件 = 2GB）⇒ 同一底座上应用必须成功
	require.NoError(t, svc.ApplyPolicyTemplate(ctx, "enterprise"))
	got, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2147483648), got.User.UserUploadSize)
	require.Equal(t, int64(2147483648), got.UploadEx.MaxFileSize)
	require.NoError(t, ValidateConfigInvariants(got))
}

// TestApplyPolicyTemplate_UnknownID 未知模板 ID ⇒ ErrPolicyInvalid（400），
// 文案列出可用 ID（可操作）。
func TestApplyPolicyTemplate_UnknownID(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()

	err := svc.ApplyPolicyTemplate(ctx, "no-such-template")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrPolicyInvalid))
	require.Contains(t, err.Error(), "intranet", "拒绝文案必须列出可用模板 ID")
}

// TestValidateConfigInvariants_Table 不变量判据表驱动（与前端 uploadCapMismatch 同口径）。
func TestValidateConfigInvariants_Table(t *testing.T) {
	cases := []struct {
		name    string
		total   int64
		perFile int64
		wantErr bool
	}{
		{"两者都 0（不限）", 0, 0, false},
		{"总量不限", 0, 4 << 30, false},
		{"单文件不限", 50 << 20, 0, false},
		{"相等（允许发一个最大文件）", 1 << 30, 1 << 30, false},
		{"总量大于单文件", 4 << 30, 1 << 30, false},
		{"总量小于单文件（矛盾）", 50 << 20, 4 << 30, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &SystemConfig{
				User:     &UserSettings{UserUploadSize: tc.total},
				UploadEx: &conf.UploadConfig{MaxFileSize: tc.perFile},
			}
			err := ValidateConfigInvariants(cfg)
			if tc.wantErr {
				require.Error(t, err)
				require.True(t, errors.Is(err, ErrPolicyInvalid))
			} else {
				require.NoError(t, err)
			}
		})
	}
	// nil 配置不 panic
	require.NoError(t, ValidateConfigInvariants(nil))
}

// TestApplyPolicyTemplate_EnvOverrideNotBroken env 恒覆盖不被模板破坏：
// env 绑定键（upload.open_upload ← PB_OPEN_UPLOAD）非空时，模板把 DB 意图改成
// true，overlay 后生效值必须仍是 env 的 false；DB 保留管理员/模板提交意图。
func TestApplyPolicyTemplate_EnvOverrideNotBroken(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	withGlobalConfForPolicy(t)

	// 注册与 bootstrap wiring 同款的 env 重施钩子（OnOverlayApplied）
	svc.SetReconfigureHooks(&ReconfigureHooks{
		OnOverlayApplied: func() { reapplyTestEnvOpenUpload() },
	})

	t.Setenv("PB_OPEN_UPLOAD", "false") // env 恒覆盖：false
	// 底座先存 open_upload=true 的意图
	require.NoError(t, svc.UpdateConfigJSON(ctx, []byte(`{"upload_ex":{"open_upload":true}}`)))

	// intranet 模板要求 open_upload=true（与 env 同向也无妨——关键是 env 施回链路活着）
	require.NoError(t, svc.ApplyPolicyTemplate(ctx, "intranet"))

	g := conf.Snapshot()
	require.NotNil(t, g)
	require.False(t, g.Upload.OpenUpload, "env 恒覆盖：生效值必须仍是 env 的 false")

	// DB 保留提交意图（模板值），与运行态分离是既有语义
	persisted, err := svc.loadPersistedConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, persisted)
	require.True(t, persisted.UploadEx.OpenUpload, "DB 应保留模板提交意图 true")

	// 管理台读到的是生效值（镜像回填）⇒ false
	got, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	require.False(t, got.UploadEx.OpenUpload, "管理台读到的应是 env 生效值")
}

// reapplyTestEnvOpenUpload 测试版 env 重施：只施 upload.open_upload 一个键
// （生产版 bootstrap.ReapplyEnvOverrides 反射全表；这里不引 bootstrap 避免包环）。
func reapplyTestEnvOpenUpload() {
	v := ""
	for _, k := range []string{"PB_OPEN_UPLOAD", "OPEN_UPLOAD"} {
		if e := getenvForTest(k); e != "" {
			v = e
			break
		}
	}
	if v == "" {
		return
	}
	want := v == "true" || v == "1"
	conf.UpdateGlobalConfig(func(next *conf.AppConfiguration) {
		next.Upload.OpenUpload = want
	})
}

// getenvForTest os.Getenv 的小包裹（seam：便于将来替换）。
func getenvForTest(k string) string {
	return os.Getenv(k)
}

// TestApplyPolicyTemplate_SeedMissingSections 全新站点（无 system_configs 记录）
// 首打模板：底座缺 upload_ex/download/transfer 段 ⇒ 必须用生效值补种，
// 模板未提及的字段不得被清零（2026-10-23 段内清零坑的「缺段」变体）。
func TestApplyPolicyTemplate_SeedMissingSections(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	withGlobalConfForPolicy(t)

	// 全局 conf 里预置「模板不碰」的字段：魔数校验开、分块关、下载超时 60
	conf.UpdateGlobalConfig(func(next *conf.AppConfiguration) {
		next.Upload.EnableMagicCheck = true
		next.Upload.EnableChunk = false
		next.Upload.TextMaxBytes = 12345
		next.Download.DownloadTimeout = 60
	})

	require.NoError(t, svc.ApplyPolicyTemplate(ctx, "intranet"))
	got, err := svc.GetConfig(ctx)
	require.NoError(t, err)

	// 模板下发的字段生效
	require.True(t, got.UploadEx.OpenUpload)
	// 模板没碰的字段必须保住生效值（不是零值）
	require.True(t, got.UploadEx.EnableMagicCheck, "模板未提及的 enable_magic_check 不得被清零")
	require.Equal(t, int64(12345), got.UploadEx.TextMaxBytes, "模板未提及的 text_max_bytes 不得被清零")
	require.Equal(t, 60, got.Download.DownloadTimeout, "模板未提及的 download_timeout 不得被清零")
}
