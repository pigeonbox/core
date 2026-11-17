package config

// 管理台目录策略段（share 段）的 overlay 语义断言（core#159）。
//
// 守卫点：
//  1. 段缺省 = **不改该段**（不是清空）——管理员保存别的分区不得冲掉目录策略；
//  2. 段存在 = **整段替换**（列表整体替换，不逐条 merge）；
//  3. 生效优先级：管理台已配置 ⇒ 以管理台为准；未配置 ⇒ 用 yaml。
//  4. 非法策略（root 相对路径 / 负配额 / 未知 share_mode）在保存时被拒。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/conf"
)

// withYAMLShare 装一份 yaml 侧的目录策略（全局 conf 的初值）。
func withYAMLShare(t *testing.T, policies ...conf.DirectoryPolicyConfig) {
	t.Helper()
	old := conf.GetGlobalConfig()
	t.Cleanup(func() { conf.SetGlobalConfig(old) })
	base := &conf.AppConfiguration{}
	if old != nil {
		copied := *old
		base = &copied
	}
	base.Share = conf.ShareConfig{DirectoryPolicies: policies}
	conf.SetGlobalConfig(base)
}

// TestUpdateConfigJSON_ShareSegmentAbsentKeepsExisting 段缺省 ⇒ 不改该段。
//
// 事故形态对齐（2026-10-05 UploadEx 冲掉 local_import.roots）：本段独立成段
// 正是为了避开「管理员保存别的分区把目录策略冲掉」。
func TestUpdateConfigJSON_ShareSegmentAbsentKeepsExisting(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	withYAMLShare(t)

	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		Share: &conf.ShareConfig{DirectoryPolicies: []conf.DirectoryPolicyConfig{
			{Root: "/data/music", QuotaBytes: 1024, ShareMode: "deny"},
		}},
	}))

	// 保存**别的**分区（不带 share 段）
	require.NoError(t, svc.UpdateConfigJSON(ctx, []byte(`{"upload_ex":{"max_file_size":1024}}`)))

	got, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, got.Share, "段缺省不得把 share 段清掉")
	require.Len(t, got.Share.DirectoryPolicies, 1)
	assert.Equal(t, "/data/music", got.Share.DirectoryPolicies[0].Root)
	assert.Equal(t, int64(1024), got.Share.DirectoryPolicies[0].QuotaBytes)
	assert.Equal(t, "deny", got.Share.DirectoryPolicies[0].ShareMode)
}

// TestUpdateConfigJSON_ShareSegmentReplacesWholesale 段存在 ⇒ 整段替换。
func TestUpdateConfigJSON_ShareSegmentReplacesWholesale(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	withYAMLShare(t)

	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		Share: &conf.ShareConfig{DirectoryPolicies: []conf.DirectoryPolicyConfig{
			{Root: "/data/music", QuotaBytes: 1024, ShareMode: "deny"},
			{Root: "/data/video", QuotaBytes: 2048, ShareMode: "allow"},
		}},
	}))

	// 提交一份全新的清单 ⇒ 整体替换（旧的 /data/video 不得残留）
	require.NoError(t, svc.UpdateConfigJSON(ctx,
		[]byte(`{"share":{"directory_policies":[{"root":"/data/pics","quota_bytes":8,"share_mode":"allow"}]}}`)))

	got, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, got.Share)
	require.Len(t, got.Share.DirectoryPolicies, 1, "列表必须整体替换（不做逐条 merge）")
	assert.Equal(t, "/data/pics", got.Share.DirectoryPolicies[0].Root)
	assert.Equal(t, int64(8), got.Share.DirectoryPolicies[0].QuotaBytes)

	// 提交空列表 ⇒ 清空该段策略清单（管理台已配置、清单为空）
	require.NoError(t, svc.UpdateConfigJSON(ctx,
		[]byte(`{"share":{"directory_policies":[]}}`)))
	got, err = svc.GetConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, got.Share)
	assert.Empty(t, got.Share.DirectoryPolicies)
}

// TestShareSegment_PriorityYamlVsAdmin 生效优先级：管理台已配置 ⇒ 管理台；
// 未配置 ⇒ yaml。
func TestShareSegment_PriorityYamlVsAdmin(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	withYAMLShare(t, conf.DirectoryPolicyConfig{Root: "/yaml/dir", QuotaBytes: 111, ShareMode: "allow"})

	// 未配置 ⇒ 生效值 = yaml
	eff := EffectiveShareSegment()
	require.NotNil(t, eff)
	require.Len(t, eff.DirectoryPolicies, 1)
	assert.Equal(t, "/yaml/dir", eff.DirectoryPolicies[0].Root, "管理台未配置 ⇒ 用 yaml")

	// 管理台配置 ⇒ overlay 生效 ⇒ 生效值 = 管理台
	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		Share: &conf.ShareConfig{DirectoryPolicies: []conf.DirectoryPolicyConfig{
			{Root: "/admin/dir", QuotaBytes: 222, ShareMode: "deny"},
		}},
	}))
	eff = EffectiveShareSegment()
	require.NotNil(t, eff)
	require.Len(t, eff.DirectoryPolicies, 1)
	assert.Equal(t, "/admin/dir", eff.DirectoryPolicies[0].Root, "管理台已配置 ⇒ 以管理台为准")
	assert.Equal(t, int64(222), eff.DirectoryPolicies[0].QuotaBytes)
}

// TestUpdateConfigJSON_ShareSegmentInvalidRejected 非法策略在保存时被拒。
func TestUpdateConfigJSON_ShareSegmentInvalidRejected(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	withYAMLShare(t)

	cases := []struct {
		name string
		body string
		want string
	}{
		{"相对路径 root", `{"share":{"directory_policies":[{"root":"rel/dir"}]}}`, "绝对路径"},
		{"负配额", `{"share":{"directory_policies":[{"root":"/abs","quota_bytes":-1}]}}`, "quota_bytes"},
		{"未知 share_mode", `{"share":{"directory_policies":[{"root":"/abs","share_mode":"dny"}]}}`, "share_mode"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := svc.UpdateConfigJSON(ctx, []byte(c.body))
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrSharePolicyInvalid)
			assert.Contains(t, err.Error(), c.want)
		})
	}
}
