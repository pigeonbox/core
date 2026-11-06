package admin

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/model"
)

// 「软删 → 回收站 → 彻底删除」必须清掉**软删子行**的物理对象。
//
// 缺陷（与 filecode_softdelete_cascade_test.go 同一批）：删除侧此前把「软删子行」
// 绑在 app 层物理清理分支里，而 PurgeFiles 列子行用的 ListByFileCodeID 是**默认
// 作用域**（软删行自动排除）⇒ 对「管理端删（子行已软删）→ 回收站彻底删除」这条
// 最常见路径，子行的物理对象**永远不会被 purge 到**（本地靠每日对账兜底，
// 远端后端 = 永久残留，2026-10-06 215 COS 实录同款后果）。
//
// ⇒ 删除/清理流的子行列举一律 Unscoped（软删行也要处理物理对象）。
//
// 负对照：把 PurgeFiles 里的 ListByFileCodeIDIncludingDeleted 退回
// ListByFileCodeID，本用例即转红（deleted 里不含子对象路径）。
func TestPurgeFiles_DeletesObjectsOfSoftDeletedChildren(t *testing.T) {
	g := newRestoreTestDB(t)
	svc := NewService(g)
	st := &mockStorage{}
	svc.SetStorage(st)
	ctx := context.Background()

	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
		Code: "PRG0001", FilePath: "uploads/2026/11/06/a.txt", Text: "a.txt",
		Size: 11, ExpiredCount: -1,
	}))
	fc, err := svc.fileCodeRepo.GetByCode(ctx, "PRG0001")
	require.NoError(t, err)
	require.NoError(t, svc.fileFileRepo.CreateBatch(ctx, []*model.FileCodeFile{
		{FileCodeID: fc.ID, FileName: "b.txt", UUIDFileName: "b.txt", FilePath: "uploads/2026/11/06/b.txt", Size: 22, SortOrder: 1},
		{FileCodeID: fc.ID, FileName: "c.txt", UUIDFileName: "c.txt", FilePath: "uploads/2026/11/06/c.txt", Size: 33, SortOrder: 2},
	}))

	// 形状＝管理端批量删除后躺在回收站里的行：主行软删 + 子行软删
	// （显式构造该形状，隔离 PurgeFiles 的子行列举行为；物理清理故意不跑——
	// 断言的是 purge 自己要清掉这些对象）。
	require.NoError(t, svc.fileCodeRepo.DeleteByIDTx(ctx, fc.ID))
	require.NoError(t, svc.fileFileRepo.SoftDeleteByFileCodeIDs(ctx, []uint{fc.ID}))
	children, err := svc.fileFileRepo.ListByFileCodeID(ctx, fc.ID)
	require.NoError(t, err)
	require.Empty(t, children, "前置条件：子行已软删（默认作用域查不到）")

	// 彻底删除：物理对象必须全清（主表路径 + 两个软删子行路径）
	purged, err := svc.PurgeFiles(ctx, []uint{fc.ID})
	require.NoError(t, err)
	require.Equal(t, int64(1), purged)

	assert.Contains(t, st.deleted, "uploads/2026/11/06/a.txt", "主表对象应被 purge")
	assert.Contains(t, st.deleted, "uploads/2026/11/06/b.txt",
		"软删子行的物理对象也必须被 purge（否则远端后端永久残留）")
	assert.Contains(t, st.deleted, "uploads/2026/11/06/c.txt",
		"软删子行的物理对象也必须被 purge（否则远端后端永久残留）")
}

// 负对照：无存储（storage==nil）时管理端批量软删也必须带走子行——
// 级联在 dao（同事务），不得绑在物理清理分支上。
func TestBatchDeleteFiles_SoftDeletesChildrenWithoutStorage(t *testing.T) {
	g := newRestoreTestDB(t)
	svc := NewService(g) // 不 SetStorage
	ctx := context.Background()

	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
		Code: "PRG0002", FilePath: "uploads/2026/11/06/d.txt", Text: "d.txt",
		Size: 11, ExpiredCount: -1,
	}))
	fc, err := svc.fileCodeRepo.GetByCode(ctx, "PRG0002")
	require.NoError(t, err)
	require.NoError(t, svc.fileFileRepo.CreateBatch(ctx, []*model.FileCodeFile{
		{FileCodeID: fc.ID, FileName: "e.txt", UUIDFileName: "e.txt", FilePath: "uploads/2026/11/06/e.txt", Size: 22, SortOrder: 1},
	}))

	n, err := svc.BatchDeleteFiles(ctx, []uint{fc.ID})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	children, err := svc.fileFileRepo.ListByFileCodeID(ctx, fc.ID)
	require.NoError(t, err)
	assert.Empty(t, children, "storage==nil 时软删也必须级联子行（级联在 dao，不在物理清理分支）")
}
