package preview

// 预览引擎单例的获取语义（2026-10-28 真机实证：fnOS 4.0.7）。
//
// ## 起因
//
// GetService() 此前在 svc==nil 时会**静默**用默认配置（缓存目录
// "./data/previews"，按进程 CWD 解析）重试初始化。NAS 包/容器/systemd 部署下
// 该目录必然不可建 ⇒ 重试必然失败 ⇒ 调用方拿到 nil 却毫无线索；更糟的是
// MaxFileSize() 的 nil 接收者分支返回 0（=不限），把 2026-10-08 加固的
// 「预览大小上限」**静默关掉**。
//
// 改为：不自动重试；未注入即返回 nil，由调用方 fail-fast 报明确错误
// （与仓内 getShareService() 对残废实例的处理同构）。
//
// ## 判据
//
// 未注入 ⇒ GetService() 返回 nil（不产生任何副作用目录）；注入 ⇒ 原样返回。

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGetService_NoSilentReinit 未注入时不得静默重试（更不得留下目录副作用）。
func TestGetService_NoSilentReinit(t *testing.T) {
	orig := svc
	t.Cleanup(func() { svc = orig })

	SetService(nil)
	require.Nil(t, GetService(), "未注入时 GetService 应返回 nil，不得用默认配置静默重试")
}

// TestSetService_RoundTrip 注入后可原样取回，且上限值可读。
func TestSetService_RoundTrip(t *testing.T) {
	orig := svc
	t.Cleanup(func() { svc = orig })

	s := NewService(&Config{
		EnablePreview:    true,
		MaxFileSize:      1234,
		PreviewCachePath: t.TempDir(),
	})
	SetService(s)
	require.Same(t, s, GetService())
	require.EqualValues(t, 1234, GetService().MaxFileSize())
}
