// 单文件直传的失败回滚（2026-11-18）
//
// 缺陷形态：单文件直传通道的顺序是「落盘 → 建分享（内含配额闸）」。建分享失败
// （配额超限 / 审核拒绝 / 落库失败）时，**文件已经躺在存储里**，而这条通道
// 没有失败回滚（同位置的多文件通道有 cleanupStored）⇒ 每次配额拒绝都留一份
// 孤儿文件。
//
// 危害不止「占磁盘」：物理用量口径是**按 file_codes 行的路径合计**
// （GetPhysicalObjectUsage）⇒ 孤儿文件**不计入配额账本** ⇒ 磁盘占用可以
// 无限超出 storage.quota（配额被绕过），直到 janitor 对账清理。
//
// 本文件断言服务层行为（回滚删什么 / 不删什么）；handler 层的「错误路径确实
// 调了回滚」由 gen/handler/share/upload_rollback_guard_test.go 单独守卫。
//
// 负对照（怎么让它变红）：
//   - 把 RollbackUploadedFile 里的 deletePhysicalIfUnreferenced 去掉
//     ⇒ 「未被引用 ⇒ 已删除」用例红；
//   - 把 deletePhysicalIfUnreferenced 换成裸 s.storage.DeleteFile
//     ⇒ 「仍被引用 ⇒ 保留」用例红（会删掉别人的共享对象）。
package share

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/model"
)

// TestRollbackUploadedFile_UnreferencedDeleted 未被任何分享引用的路径 ⇒ 删掉。
func TestRollbackUploadedFile_UnreferencedDeleted(t *testing.T) {
	svc, _, st, _, _ := newTestServiceWithDB(t)
	ctx := context.Background()

	svc.RollbackUploadedFile(ctx, "uploads/2026/11/18/orphan.bin")

	require.True(t, st.deleteCalled, "未被引用的刚落盘文件必须被回滚删除（否则配额可被绕过）")
	require.Equal(t, "uploads/2026/11/18/orphan.bin", st.deletedPath)
}

// TestRollbackUploadedFile_ReferencedKept 仍被别的分享引用（秒传共享对象）⇒ 保留。
func TestRollbackUploadedFile_ReferencedKept(t *testing.T) {
	svc, gormDB, st, _, _ := newTestServiceWithDB(t)
	ctx := context.Background()

	shared := "uploads/2026/11/18/shared.bin"
	require.NoError(t, gormDB.Create(&model.FileCode{
		Code: "SHARED01", FilePath: shared, Size: 1, Text: "shared.bin",
		ExpiredCount: -1,
	}).Error)

	svc.RollbackUploadedFile(ctx, shared)

	require.False(t, st.deleteCalled,
		"仍被其它分享引用的物理文件不得被回滚删除（会删掉别人的秒传共享对象）")
}

// TestRollbackUploadedFile_EmptyPathNoop 空路径 ⇒ 不做任何删除（防御）。
func TestRollbackUploadedFile_EmptyPathNoop(t *testing.T) {
	svc, _, st, _, _ := newTestServiceWithDB(t)

	svc.RollbackUploadedFile(context.Background(), "")

	require.False(t, st.deleteCalled, "空路径不应触发删除")
}
