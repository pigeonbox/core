package preview

// 预览引擎不可用时必须 fail-fast，且**不得触碰文件内容**（2026-10-28 真机实证：
// fnOS 4.0.7 因缓存目录不可建 ⇒ 预览引擎 init 失败 ⇒ GetService() 返回 nil）。
//
// ## 为什么这条是安全红线而不只是功能缺陷
//
// 引擎不可用 ⇒ MaxFileSize() 走 nil 接收者分支返回 0（=**不限**）⇒
// 2026-10-08 加固的「预览大小上限」被静默关掉；而若 nil 检查排在「文件全量
// 暂存到临时盘」之后，则任意大文件仍会先落满临时盘才报错 ⇒ 磁盘耗尽 DoS 面
// 回归（这条加固正是当年为堵这个面加的）。
//
// ## 判据
//
// 注入 nil 引擎后，generatePreview 必须在**触碰存储之前**失败：storageSvc 为
// nil 仍未被触达即为证（若走到存储分支，报错会是 "storage not available"）。

import (
	"context"
	"testing"

	previewService "github.com/pigeonbox/core/preview"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/require"
)

// TestGeneratePreview_EngineUnavailable_FailsFast 引擎不可用 ⇒ 立即失败且不碰存储。
func TestGeneratePreview_EngineUnavailable_FailsFast(t *testing.T) {
	orig := previewService.GetService()
	t.Cleanup(func() { previewService.SetService(orig) })
	previewService.SetService(nil)

	svc := NewService(newPreviewTestDB(t), nil)
	_, err := svc.generatePreview(context.Background(), &model.FileCode{
		FilePath: "uploads/2026/10/28/z.txt",
		Size:     200,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "preview service not available")
	require.NotContains(t, err.Error(), "storage not available",
		"引擎不可用时不得走到存储分支（否则大文件已先全量落盘）")
}
