// 秒传/去重行为测试（2026-10-12，D1=A 同用户内去重）：
//  1. CreateShare 去重钩子：同用户同 hash+size 命中 → 冗余副本回收 + 新分享引用源对象；
//  2. 跨用户不去重（存在性 oracle 防护）；
//  3. 物理删除保护：共享对象被删除分享的物理清理跳过，引用清零后才真删。
package share

import (
	"context"
	"testing"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func dedupeReq(userID *uint, path, hash, name string, size int64) *ShareFileReq {
	return &ShareFileReq{
		Channel:      "test",
		FilePath:     path,
		Size:         size,
		Text:         name,
		ExpiredCount: -1,
		UserID:       userID,
		UploadType:   "authenticated",
		FileHash:     hash,
	}
}

func TestCreateShare_DedupeReusesSourceObject(t *testing.T) {
	svc, _, st, _, _ := newTestServiceWithDB(t)
	ctx := context.Background()
	u := uint(1)

	a, err := svc.CreateShare(ctx, dedupeReq(&u, "uploads/a.bin", "h1", "report-a.pdf", 10))
	require.NoError(t, err)
	assert.False(t, a.Deduped, "首个分享不去重")
	assert.Equal(t, "uploads/a.bin", a.FilePath)

	// 同用户同内容再次分享（模拟已落盘的新副本 uploads/b.bin）
	b, err := svc.CreateShare(ctx, dedupeReq(&u, "uploads/b.bin", "h1", "report-b.pdf", 10))
	require.NoError(t, err)
	assert.True(t, b.Deduped, "同用户同 hash+size 应命中秒传")
	assert.Equal(t, "uploads/a.bin", b.FilePath, "新分享应引用源对象路径")
	assert.Contains(t, st.deletedPaths, "uploads/b.bin", "冗余新副本应被回收")
	assert.NotEqual(t, a.Code, b.Code, "两次分享各有独立取件码")
}

func TestCreateShare_DedupeSkipsCrossUser(t *testing.T) {
	svc, _, st, _, _ := newTestServiceWithDB(t)
	ctx := context.Background()
	u1, u2 := uint(1), uint(2)

	_, err := svc.CreateShare(ctx, dedupeReq(&u1, "uploads/a.bin", "h1", "x.pdf", 10))
	require.NoError(t, err)

	// 其他用户的同内容文件：不去重、不回收其副本（存在性 oracle 防护）
	b, err := svc.CreateShare(ctx, dedupeReq(&u2, "uploads/b.bin", "h1", "x.pdf", 10))
	require.NoError(t, err)
	assert.False(t, b.Deduped, "跨用户不得去重")
	assert.Equal(t, "uploads/b.bin", b.FilePath)
	assert.NotContains(t, st.deletedPaths, "uploads/b.bin", "跨用户副本不得回收")
}

func TestCreateShare_DedupeNeverDeletesReferencedPath(t *testing.T) {
	// 回归（2026-10-12 实施自查）：presign/chunk 秒传建分享传入的 req.FilePath
	// 可能本身就是既有源的路径（被其分享行引用）；两查之间出现更新的源而发生
	// 路径切换时，被引用路径绝不能当"冗余副本"删除。
	svc, _, st, _, _ := newTestServiceWithDB(t)
	ctx := context.Background()
	u := uint(1)

	a, err := svc.CreateShare(ctx, dedupeReq(&u, "uploads/a.bin", "h1", "x.pdf", 10))
	require.NoError(t, err)

	// 模拟并发上传抢先建出的更新源（绕过 CreateShare 钩子直插，路径独立）
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
		Code: "RACEB01", FilePath: "uploads/b.bin", FileHash: "h1", Size: 10,
		Text: "x.pdf", Status: model.StatusNormal, ExpiredCount: -1, UserID: &u,
	}))

	// 模拟秒传建分享：以 A 的路径起手（lookup 当时选中 A），钩子复查找到更新的 B
	c, err := svc.CreateShare(ctx, dedupeReq(&u, "uploads/a.bin", "h1", "x.pdf", 10))
	require.NoError(t, err)
	assert.True(t, c.Deduped)
	assert.Equal(t, "uploads/b.bin", c.FilePath, "应切换到最新源路径")
	assert.NotContains(t, st.deletedPaths, "uploads/a.bin", "被 A 引用的路径不得回收")
	assert.NotContains(t, st.deletedPaths, "uploads/b.bin", "被 B 引用的路径不得回收")
	_ = a
}

func TestDeleteFileByCode_GuardSharedObject(t *testing.T) {
	svc, gormDB, st, _, _ := newTestServiceWithDB(t)
	ctx := context.Background()
	u := uint(1)

	a, err := svc.CreateShare(ctx, dedupeReq(&u, "uploads/a.bin", "h1", "x.pdf", 10))
	require.NoError(t, err)
	b, err := svc.CreateShare(ctx, dedupeReq(&u, "uploads/b.bin", "h1", "x.pdf", 10))
	require.NoError(t, err)
	require.True(t, b.Deduped)
	require.Equal(t, "uploads/a.bin", b.FilePath)

	// 删 A：B 仍引用共享对象 → 物理文件保留
	require.NoError(t, svc.DeleteFileByCode(ctx, a.Code, u))
	assert.NotContains(t, st.deletedPaths, "uploads/a.bin", "共享对象被引用时不得物理删除")

	// 删 B：A 在回收站（软删行）仍持有引用 → 物理文件继续保留
	require.NoError(t, svc.DeleteFileByCode(ctx, b.Code, u))
	assert.NotContains(t, st.deletedPaths, "uploads/a.bin", "回收站行仍持有物理文件")

	// 彻底清空引用（硬删两行模拟 purge 完毕）后再删 → 物理文件释放
	_, err = svc.fileCodeRepo.HardDeleteByIDs(ctx, []uint{a.ID, b.ID})
	require.NoError(t, err)
	svc.deletePhysicalIfUnreferenced(ctx, "uploads/a.bin", 0)
	assert.Contains(t, st.deletedPaths, "uploads/a.bin", "引用清零后物理文件应释放")

	// 子表行同样计入引用（多文件分享子文件）
	var count int64
	require.NoError(t, gormDB.Model(&model.FileCodeFile{}).Count(&count).Error)
}
