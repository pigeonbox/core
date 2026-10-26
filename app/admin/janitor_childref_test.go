package admin

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/pigeonbox/core/storage"
)

// TestCleanRemotePresignOrphans_ChildRowReferenceKeepsObject 回归（2026-10-26 审计）：
// 远端 presign 孤儿扫描的**引用集原先只收主表** ⇒ 多文件分享中「非首个文件」
// （只存在于 file_code_files 子表）被当孤儿**删掉** ⇒ 仍在被引用的活对象**数据丢失**。
// ⇒ 引用集必须与本地磁盘对账（ReconcileOrphans）同口径：主表 + 子表。
func TestCleanRemotePresignOrphans_ChildRowReferenceKeepsObject(t *testing.T) {
	const childOnly = "uploads/2026/10/26/up_child222.pdf"
	const orphan = "uploads/2026/10/26/up_orphan333.pdf"

	j, stub := newJanitorEnv(t, []storage.RemoteObjectInfo{
		obj(childOnly, 48*time.Hour), // 只被**子表**引用
		obj(orphan, 48*time.Hour),    // 无引用 ⇒ 真孤儿
	})
	ctx := context.Background()
	// 分享主表指向**别的**文件（非首文件在子表里 —— 这正是多文件分享的形态）
	require.NoError(t, j.fileCodeRepo.Create(ctx, &model.FileCode{
		Code: "MULTI01", FilePath: "uploads/2026/10/26/first.pdf", UUIDFileName: "first.pdf",
	}))
	// 子表行引用 up_child222.pdf
	require.NoError(t, j.fileFileRepo.CreateBatch(ctx, []*model.FileCodeFile{{
		FileCodeID:   1,
		FileName:     "child.pdf",
		UUIDFileName: "up_child222.pdf",
		FilePath:     childOnly,
	}}))

	removed, err := j.CleanRemotePresignOrphans(ctx, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, removed, "只应删掉 1 个真孤儿")

	assert.NotContains(t, stub.deleted, childOnly,
		"**子表引用**的对象绝不能被删（多文件分享的非首文件会丢文件）")
	assert.Contains(t, stub.deleted, orphan, "无引用且超宽限期的孤儿应被删")
}
