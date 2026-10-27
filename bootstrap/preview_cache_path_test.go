package bootstrap

// 预览缓存目录必须落在**已解析的数据目录**下（2026-10-28 真机实证：fnOS 4.0.7）。
//
// ## 起因
//
// bootstrap 原先把预览缓存目录硬编码成相对路径 "./data/previews"——相对路径按
// **进程 CWD** 解析，而 NAS 包（fnOS 等）、容器、systemd 部署的 CWD 都不指向
// 数据目录 ⇒ 建目录 EACCES ⇒ 预览服务 init 失败。真机日志（fnOS 4.0.7，每次
// 启动必现；且该错误自 3.3.x 起长期存在，不是新版本引入的回归）：
//
//	Failed to init preview service {"error": "failed to create preview cache
//	directory: mkdir ./data/previews: permission denied"}
//
// ## 为什么要机械守卫
//
// ① 缓存目录写回相对路径 ⇒ NAS/容器部署下预览功能整体不可用，而 CI 全绿
//    （测试都在可写的临时目录里跑，撞不到 CWD 不可写这一格）；
// ② 连带安全面：引擎 init 失败 ⇒ GetService() 返回 nil ⇒ MaxFileSize() 走
//    nil 接收者分支返回 0（=不限）⇒ 2026-10-08 加固的「预览大小上限」被静默
//    关掉 ⇒ 任意大文件仍会先全量落临时盘（磁盘耗尽 DoS 面）。
//
// ## 判据
//
// 给一个数据目录 ⇒ 缓存目录必须建在**它下面**；且不得在 CWD 旁边留下旧行为
// 的相对路径落点。

import (
	"os"
	"path/filepath"
	"testing"

	previewPkg "github.com/pigeonbox/core/preview"
	"github.com/stretchr/testify/require"
)

// TestInitPreviewService_CacheUnderDataPath 缓存目录建在已解析的数据目录下。
func TestInitPreviewService_CacheUnderDataPath(t *testing.T) {
	t.Cleanup(func() { previewPkg.SetService(nil) })

	dataPath := t.TempDir()
	require.NoError(t, initPreviewService(dataPath))

	cacheDir := filepath.Join(dataPath, "previews")
	info, err := os.Stat(cacheDir)
	require.NoError(t, err, "预览缓存目录必须建在已解析的数据目录下：%s", cacheDir)
	require.True(t, info.IsDir())

	// init 成功后引擎必须已注入（否则调用方拿到 nil，静默降级）
	require.NotNil(t, previewPkg.GetService(), "预览服务 init 成功后必须注入单例")
}

// TestInitPreviewService_NoLegacyRelativeCache 反证：旧行为（CWD 相对的
// "./data/previews"）不得在进程工作目录下留出缓存目录。
//
// 该落点正是 NAS/容器部署下不可写的那一格——一旦代码回退到相对路径，这里会
// 在包目录下留下 data/previews，从而红。
func TestInitPreviewService_NoLegacyRelativeCache(t *testing.T) {
	t.Cleanup(func() { previewPkg.SetService(nil) })

	cwd, err := os.Getwd()
	require.NoError(t, err)
	legacyDir := filepath.Join(cwd, "data", "previews")
	t.Cleanup(func() { _ = os.RemoveAll(legacyDir) })

	require.NoError(t, initPreviewService(t.TempDir()))

	_, statErr := os.Stat(legacyDir)
	require.True(t, os.IsNotExist(statErr),
		"预览缓存目录不得回退到 CWD 相对的 %s（NAS/容器部署下必然不可写）", legacyDir)
}
