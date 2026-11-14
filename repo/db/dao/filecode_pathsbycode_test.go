package dao

import (
	"context"
	"testing"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PathsByCodeForUser 判据（评审 D03）：硬删前抓该分享的全部对象路径——
//
//	① 目标行已在回收站（软删）⇒ 必须 Unscoped 才查得到；
//	② 主表 + 子表（file_code_files）全收——多文件分享的非首文件只在子表；
//	③ 只取本 code + user 的行，不串其他分享。
//
// 抓取失败由调用方记 warn 后继续删 DB（泄漏由 janitor 兜底），但抓取成功时
// 返回的路径集合必须完整——漏一条 = 该对象只能等远端 janitor 扫描。
func TestPathsByCodeForUser_CoversMainAndChildren(t *testing.T) {
	g := newGovernanceTestDB(t)
	repo := NewFileCodeRepository(g)
	childRepo := NewFileCodeFileRepository(g)
	ctx := context.Background()
	uid := uint(9)

	mainPath := "uploads/2026/11/14/pb-main.bin"
	child1 := "uploads/2026/11/14/pb-child1.bin"
	child2 := "uploads/2026/11/14/pb-child2.bin"

	fc := &model.FileCode{Code: "PB40001", UserID: &uid, FilePath: mainPath, Text: "pb-main.bin"}
	require.NoError(t, repo.Create(ctx, fc))
	require.NoError(t, childRepo.CreateBatch(ctx, []*model.FileCodeFile{
		{FileCodeID: fc.ID, FileName: "c1.bin", UUIDFileName: "c1.bin", FilePath: child1},
		{FileCodeID: fc.ID, FileName: "c2.bin", UUIDFileName: "c2.bin", FilePath: child2},
	}))

	// 他分享（同用户）——路径不得串进来
	other := &model.FileCode{Code: "PB40002", UserID: &uid, FilePath: "uploads/2026/11/14/other.bin", Text: "other.bin"}
	require.NoError(t, repo.Create(ctx, other))

	// 前置态：目标在回收站（硬删对象是软删行 ⇒ 必须含软删）
	n, err := repo.BatchSoftDeleteByCodes(ctx, uid, []string{"PB40001"})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	paths, err := repo.PathsByCodeForUser(ctx, uid, "PB40001")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{mainPath, child1, child2}, paths,
		"主表+子表路径全收（含软删行），评审 D03")

	// 他码：只回他自己的路径
	paths2, err := repo.PathsByCodeForUser(ctx, uid, "PB40002")
	require.NoError(t, err)
	assert.Equal(t, []string{"uploads/2026/11/14/other.bin"}, paths2)

	// 不存在的 code → 显式错误（调用方记 warn 后继续删 DB）
	_, err = repo.PathsByCodeForUser(ctx, uid, "NOPE9999")
	require.Error(t, err)
}
