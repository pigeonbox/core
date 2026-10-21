package share

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/conf"
)

// 回归：本地文件管理三层防穿越——root 索引只来自服务端配置、relPath 拒绝 ..、
// EvalSymlinks 复核落点（符号链接逃逸必须被拒）。
func TestLocalFilesConfinement(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "b.txt"), []byte("world"), 0o644))
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o644))
	// 符号链接：root/link → outside（逃逸尝试面）
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "link")))

	old := conf.GetGlobalConfig()
	conf.SetGlobalConfig(&conf.AppConfiguration{
		Upload: conf.UploadConfig{LocalImport: conf.LocalImportConfig{
			Enabled: true, Roots: []string{root},
		}},
	})
	t.Cleanup(func() { conf.SetGlobalConfig(old) })

	svc := NewService(newTestDB(t), "", nil)

	// 列表：根目录与子目录
	entries, err := svc.ListLocalFiles(0, "")
	require.NoError(t, err)
	require.Len(t, entries, 3) // a.txt + sub/ + link/
	entries, err = svc.ListLocalFiles(0, "sub")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "b.txt", entries[0].Name)

	// 越界与非法路径
	_, err = svc.ListLocalFiles(0, "../")
	require.Error(t, err)
	_, err = svc.ListLocalFiles(0, "sub/../../x")
	require.Error(t, err)
	_, err = svc.ListLocalFiles(9, "")
	require.Error(t, err, "root 索引越界")
	require.Error(t, svc.DeleteLocalFile(0, "/etc/passwd"), "绝对路径拒绝")

	// 符号链接逃逸：link/secret.txt 落点在 root 外，必须拒绝
	require.Error(t, svc.DeleteLocalFile(0, "link/secret.txt"))

	// Windows 形态载荷：pathx 严格档统一按分隔符判定（"\"" 亦为分隔符），
	// 且拒绝盘符/UNC 绝对形态——这些在 Linux 上按平台解释会漏判
	for _, bad := range []string{
		`..\..\etc\passwd`,     // 反斜杠形态的 ..
		`sub\..\..\x`,          // 反斜杠形态的越界
		`C:\Windows\win.ini`,   // 盘符绝对路径
		`\\host\share\secret`,  // UNC
		`sub/../b.txt`,         // 显式 ".." 段（严格档直接拒，不做抵消推断）
	} {
		_, err = svc.ListLocalFiles(0, bad)
		require.Error(t, err, "应拒绝: %s", bad)
	}

	// 合法文件名里含 ".." 不再被误杀（此前是「含 '..' 即非法」的字符串判定）
	require.NoError(t, os.WriteFile(filepath.Join(root, "a..b.txt"), []byte("ok"), 0o644))
	entries, err = svc.ListLocalFiles(0, "")
	require.NoError(t, err)
	require.Len(t, entries, 4) // a.txt + a..b.txt + sub/ + link/

	// 正常删除：仅普通文件
	require.NoError(t, svc.DeleteLocalFile(0, "a.txt"))
	_, err = os.Stat(filepath.Join(root, "a.txt"))
	require.True(t, os.IsNotExist(err))
	require.Error(t, svc.DeleteLocalFile(0, "sub"), "目录不允许删除")

	// 未启用时拒绝
	conf.SetGlobalConfig(&conf.AppConfiguration{})
	require.Error(t, func() error { _, err := svc.ListLocalFiles(0, ""); return err }())

	// 启用但未配白名单目录：报引导性错误（与「root 索引越界」分级，管理端据此展示引导态）
	conf.SetGlobalConfig(&conf.AppConfiguration{
		Upload: conf.UploadConfig{LocalImport: conf.LocalImportConfig{Enabled: true}},
	})
	_, err = svc.ListLocalFiles(0, "")
	require.ErrorContains(t, err, "白名单目录")
}
