// 单文件直传**错误路径**必须调用失败回滚（接线守卫，2026-11-18）。
//
// 背景：服务层已有 RollbackUploadedFile（app/share/rollback_uploaded_file_test.go
// 覆盖其行为），但**接线**在 HTTP 适配层：单文件直传的顺序是「落盘 → 建分享」，
// 建分享失败时若不调回滚，落盘文件就留在存储里（孤儿 ⇒ 不计入配额账本 ⇒
// 磁盘可被绕过配额撑爆）。
//
// 本守卫用**源码全文检索**（core/bootstrap/share_surface_contract_test.go 同款范式）
// 断言「ShareFile 的错误分支里确实调了 RollbackUploadedFile」——因为这条路径
// 需要 hertz 请求上下文，直接跑 handler 级集成成本高，而接线漏掉正是本缺陷
// 的主要成因。
//
// 负对照（怎么让它变红）：把 share_service.go 里错误分支的
// 「getShareService().RollbackUploadedFile(ctx, filePath)」这一行删掉
// ⇒ 本守卫红。
package share

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSingleUploadErrorPathRollsBackStoredFile 错误分支必须回滚已落盘文件。
func TestSingleUploadErrorPathRollsBackStoredFile(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "取不到本测试文件路径（runtime.Caller 失败）")

	src, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "share_service.go"))
	require.NoError(t, err, "读不到 share_service.go（源码守卫自检）")
	text := string(src)

	idx := strings.Index(text, "ShareFile(ctx, shareReq)")
	require.Positive(t, idx, "share_service.go 里找不到 ShareFile(ctx, shareReq) 调用点（源码形态变了？）")

	// 取该调用点之后的一段：错误分支就在紧随其后的 if err != nil 块里。
	segment := text[idx:minInt(len(text), idx+600)]

	require.Contains(t, segment, "RollbackUploadedFile(ctx, filePath)",
		"单文件直传的建分享错误分支必须回滚已落盘文件（否则每次配额拒绝都留孤儿；"+
			"孤儿不计入配额账本 ⇒ 磁盘可绕过 storage.quota）")
}

// minInt 取小者（避免为一个小工具引入额外依赖）。
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
