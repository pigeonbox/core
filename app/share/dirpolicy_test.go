package share

// 目录策略层断言（core#159 地基）。
//
// 守卫点：
//  1. 前缀匹配分隔符感知 + 最长前缀优先（顺序无关）；
//  2. deny 拒收、allow/空放行、未知 share_mode fail-closed；
//  3. 本地导入闸接线：deny 在落盘前拒收（文案定位到目录），allow 不误伤。

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/conf"
)

func TestMatchDirectoryPolicy(t *testing.T) {
	policies := []conf.DirectoryPolicyConfig{
		{Root: "/data/music"},
		{Root: "/data/music/jazz"},
		{Root: "/data/music2"},
		{Root: "relative/path"}, // 非绝对路径：跳过（不匹配任何绝对路径）
	}

	// 精确命中
	assert.Equal(t, "/data/music", matchDirectoryPolicy(policies, "/data/music").Root)
	// 子路径命中（父策略）
	assert.Equal(t, "/data/music", matchDirectoryPolicy(policies, "/data/music/album/a.flac").Root)
	// 最长前缀优先（与配置顺序无关：jazz 条目在列表中间也必须赢）
	assert.Equal(t, "/data/music/jazz", matchDirectoryPolicy(policies, "/data/music/jazz/x.flac").Root)
	// 分隔符边界：/data/music2 不得被 /data/music 命中
	assert.Equal(t, "/data/music2", matchDirectoryPolicy(policies, "/data/music2/y.flac").Root)
	// 无命中
	assert.Nil(t, matchDirectoryPolicy(policies, "/other/x"))
	// 空策略
	assert.Nil(t, matchDirectoryPolicy(nil, "/data/music"))
	// 只剩非绝对路径条目 ⇒ 不命中（不 panic、不误配）
	assert.Nil(t, matchDirectoryPolicy([]conf.DirectoryPolicyConfig{{Root: "relative/path"}}, "/relative/path/x"))
	// 根 "/" 匹配一切绝对路径
	assert.Equal(t, "/", matchDirectoryPolicy([]conf.DirectoryPolicyConfig{{Root: "/"}}, "/any/where").Root)
}

func TestCheckDirectoryPolicy(t *testing.T) {
	// 真实目录 + 真实文件：checkDirectoryPolicy 入口做 EvalSymlinks 解析
	//（macOS /var 是符号链接，两侧解析必须同口径才命中）
	root := t.TempDir()
	child := filepath.Join(root, "album")
	require.NoError(t, os.MkdirAll(child, 0o755))
	pathIn := filepath.Join(child, "a.flac")
	require.NoError(t, os.WriteFile(pathIn, []byte("x"), 0o644))
	rootReal, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)

	// 无策略 ⇒ 放行
	require.NoError(t, checkDirectoryPolicy(nil, pathIn))

	// deny ⇒ 拒绝且文案定位到目录（root 已解析形态）
	err = checkDirectoryPolicy([]conf.DirectoryPolicyConfig{
		{Root: root, ShareMode: "deny"},
	}, pathIn)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "禁止生成分享")
	assert.Contains(t, err.Error(), rootReal, "错误文案必须定位到具体目录")

	// allow / 空 ⇒ 放行
	for _, mode := range []string{"allow", "ALLOW ", ""} {
		require.NoError(t, checkDirectoryPolicy([]conf.DirectoryPolicyConfig{
			{Root: root, ShareMode: mode},
		}, pathIn), "mode=%q", mode)
	}

	// 未命中策略的目录不受 deny 影响
	other := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(other, "a.flac"), []byte("x"), 0o644))
	require.NoError(t, checkDirectoryPolicy([]conf.DirectoryPolicyConfig{
		{Root: root, ShareMode: "deny"},
	}, filepath.Join(other, "a.flac")))

	// 不存在的策略目录：解析层跳过 ⇒ 不命中（不误伤）
	require.NoError(t, checkDirectoryPolicy([]conf.DirectoryPolicyConfig{
		{Root: filepath.Join(root, "does-not-exist"), ShareMode: "deny"},
	}, pathIn))

	// 非绝对路径策略：不生效（也不得按 CWD 误解析）
	require.NoError(t, checkDirectoryPolicy([]conf.DirectoryPolicyConfig{
		{Root: "relative/path", ShareMode: "deny"},
	}, pathIn))

	// 未知 share_mode ⇒ fail-closed（拼错不得静默放行）
	err = checkDirectoryPolicy([]conf.DirectoryPolicyConfig{
		{Root: root, ShareMode: "dny"},
	}, pathIn)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "share_mode 非法")
}

// TestImportLocalFile_DirectoryPolicy 本地导入闸接线（core#159）。
func TestImportLocalFile_DirectoryPolicy(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "evil.exe"), []byte("MZ"), 0o644))

	old := conf.GetGlobalConfig()
	conf.SetGlobalConfig(&conf.AppConfiguration{
		Upload: conf.UploadConfig{LocalImport: conf.LocalImportConfig{
			Enabled: true, Roots: []string{root},
		}},
		Share: conf.ShareConfig{DirectoryPolicies: []conf.DirectoryPolicyConfig{
			{Root: root, ShareMode: "deny"},
		}},
	})
	t.Cleanup(func() { conf.SetGlobalConfig(old) })

	svc := NewService(newTestDB(t), "", nil) // storage=nil：deny 必须在落盘前拒收，绝不触到存储

	// deny ⇒ 拒收且定位到目录（零副作用：发生在 os.Open/SaveStream 之前）
	_, err := svc.ImportLocalFile(context.Background(), ImportLocalOpts{AbsPath: filepath.Join(root, "a.txt")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "禁止生成分享")

	// allow ⇒ 闸放行（走到扩展名黑名单才被拒——证明策略不误伤；
	// 不用合法文件走全程：storage=nil 会在落盘阶段 panic，本用例只验闸口）
	conf.SetGlobalConfig(&conf.AppConfiguration{
		Upload: conf.UploadConfig{LocalImport: conf.LocalImportConfig{
			Enabled: true, Roots: []string{root},
		}},
		Share: conf.ShareConfig{DirectoryPolicies: []conf.DirectoryPolicyConfig{
			{Root: root, ShareMode: "allow"},
		}},
	})
	_, err = svc.ImportLocalFile(context.Background(), ImportLocalOpts{AbsPath: filepath.Join(root, "evil.exe")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "文件类型", "allow 模式必须穿过策略闸（落到既有的扩展名检查）")
	assert.NotContains(t, err.Error(), "禁止生成分享")
}
