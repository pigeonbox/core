package dao

// 用户存储占用的**批量实时聚合**守卫（GetTotalSizesByUserIDs）。
//
// ## 为什么要有
//
// 管理台用户页每行都要显示「已用空间」，若逐个用户查（GetTotalSizeByUserID）
// 就是 N+1；但为了省这一下而改读 `users.total_storage` 计数器会引入更严重的
// 问题——那个计数器只增不减（删分享/管理员删用户都不会回退），线上实录过
// 「计数器 4.9GB 而 file_codes 里 0 行」。配额判定 CheckQuota 用的是实时聚合，
// 两个真相源必然互相矛盾。
//
// ## 守卫点
//
//  1. 按 user_id 分组求和，且**软删的分享不计入**（与单用户口径一致）；
//  2. 没有分享记录的用户**不出现在 map 里**（调用方按 0 处理，不是「查不到」）；
//  3. 匿名声明的 user_id 为 NULL，绝不能被算到某个真实用户名下。

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetTotalSizesByUserIDs(t *testing.T) {
	g := newGovernanceTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	require.NoError(t, repo.Create(ctx, &model.FileCode{Code: "U1AAA001", Size: 100, UserID: uintPtr(1)}))
	require.NoError(t, repo.Create(ctx, &model.FileCode{Code: "U1AAA002", Size: 200, UserID: uintPtr(1)}))
	// 用户 2 的唯一一条分享随后被软删 ⇒ 占用必须归 0（不回退计数器、也不报错）
	deleted := &model.FileCode{Code: "U2AAA001", Size: 50, UserID: uintPtr(2)}
	require.NoError(t, repo.Create(ctx, deleted))
	require.NoError(t, repo.Delete(ctx, deleted.ID))
	// 匿名分享（user_id = NULL）不得计入任何用户
	require.NoError(t, repo.Create(ctx, &model.FileCode{Code: "ANON0001", Size: 999, UserID: nil}))

	sizes, err := repo.GetTotalSizesByUserIDs(ctx, []uint{1, 2, 3})
	require.NoError(t, err)

	assert.EqualValues(t, 300, sizes[1], "同用户多条分享必须求和")
	_, has2 := sizes[2]
	assert.False(t, has2, "全被软删的用户不应出现（调用方按 0 处理）")
	_, has3 := sizes[3]
	assert.False(t, has3, "没有任何分享的用户不应出现")
	assert.Equal(t, 1, len(sizes), "匿名 NULL 行不得被算到任何用户名下")

	// 与单用户口径逐字一致（两处必须同源，否则列表与配额判定再次分裂）
	single, err := repo.GetTotalSizeByUserID(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, single, sizes[1])

	// 空入参不查库、不报错（列表页可能一页 0 条）
	empty, err := repo.GetTotalSizesByUserIDs(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// 评审 D01 + core#193 正名：逻辑分享容量必须含回收站软删行（purge 前分享行
// 仍在、用量不回退），与 InsightStats 的 Unscoped 口径一致（core#107 同族）。
func TestGetLogicalShareSizeIncludingDeleted_CountsSoftDeleted(t *testing.T) {
	g := newGovernanceTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	alive := &model.FileCode{Code: "PHYSAAA1", Size: 1000}
	soft := &model.FileCode{Code: "PHYSAAA2", Size: 2000}
	require.NoError(t, repo.Create(ctx, alive))
	require.NoError(t, repo.Create(ctx, soft))
	require.NoError(t, repo.Delete(ctx, soft.ID)) // 进回收站

	if got, err := repo.GetTotalSize(ctx); err != nil || got != 1000 {
		t.Fatalf("存活口径应=1000（断言不改）, got %d err=%v", got, err)
	}
	logical, err := repo.GetLogicalShareSizeIncludingDeleted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if logical != 3000 {
		t.Fatalf("逻辑口径应=3000（含软删）, got %d", logical)
	}
}

// core#193 回归：物理对象占用按存储路径去重。
//
// 判据（issue 验收）：同一文件 100 次分享 ⇒ 物理占用 = 1 份，逻辑用量 = 100 份；
// 软删（回收站）、跨表（主表 ∪ 子表）、多引用三种组合逐一覆盖。
func TestGetPhysicalObjectUsage_DedupAndTrash(t *testing.T) {
	g := newGovernanceTestDB(t)
	repo := NewFileCodeRepository(g)
	fileFileRepo := NewFileCodeFileRepository(g)
	ctx := context.Background()

	const sharedPath = "uploads/2026/11/16/shared.bin"
	// 同一路径 100 个分享（秒传共享形态），每行 size=10 ⇒ 逻辑=1000
	for i := 0; i < 100; i++ {
		require.NoError(t, repo.Create(ctx, &model.FileCode{
			Code: fmt.Sprintf("DEDU%04d", i), FilePath: sharedPath, Size: 10,
		}))
	}
	// 主表另一份独立对象 + 一条子表行（独立对象）——去重基线 = 3 个物理对象
	require.NoError(t, repo.Create(ctx, &model.FileCode{
		Code: "DEDUALONE", FilePath: "uploads/alone.bin", Size: 100,
	}))
	require.NoError(t, repo.Create(ctx, &model.FileCode{
		Code: "DEDUWCHR", FilePath: "uploads/parent.bin", Size: 50,
	}))
	parent, err := repo.GetByCode(ctx, "DEDUWCHR")
	require.NoError(t, err)
	require.NoError(t, fileFileRepo.CreateBatch(ctx, []*model.FileCodeFile{
		{FileCodeID: parent.ID, FileName: "c.bin", FilePath: "uploads/child.bin", Size: 60},
	}))

	// 跨表同路径不双计：子行路径与主表 alone.bin 相同（脏数据/历史形态防御）
	require.NoError(t, fileFileRepo.CreateBatch(ctx, []*model.FileCodeFile{
		{FileCodeID: parent.ID, FileName: "dup.bin", FilePath: "uploads/alone.bin", Size: 100},
	}))

	logical, err := repo.GetLogicalShareSizeIncludingDeleted(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 1000+100+50, logical, "逻辑口径=SUM(file_codes.size)，未去重（子表行不在主表合计内）")

	physical, err := repo.GetPhysicalObjectUsage(ctx)
	require.NoError(t, err)
	// 去重后：shared.bin(10) + alone.bin(100，主子表双写只计一次) + parent.bin(50) + child.bin(60)
	assert.EqualValues(t, 10+100+50+60, physical, "物理口径必须按路径去重（主表 ∪ 子表）")

	// 回收站软删仍占盘：软删一个独立对象 ⇒ 物理占用不变；再软删全部 shared 行同理
	alone, err := repo.GetByCode(ctx, "DEDUALONE")
	require.NoError(t, err)
	require.NoError(t, repo.Delete(ctx, alone.ID))
	physical, err = repo.GetPhysicalObjectUsage(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 10+100+50+60, physical, "软删行（回收站）purge 前仍占盘，物理口径不回退")

	// 同路径尺寸漂移（脏数据）：取 MAX 确定值，不放大
	require.NoError(t, repo.Create(ctx, &model.FileCode{
		Code: "DEDUDRIFT", FilePath: sharedPath, Size: 99,
	}))
	physical, err = repo.GetPhysicalObjectUsage(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 10+100+50+60+89, physical, "shared.bin 漂移后取 MAX(99)——仅 shared 项从 10 变 99")
}

// core#193 守卫：旧名不得复活。GetTotalSizeIncludingDeleted 的「物理口径」
// 名不副实（未去重的逻辑合计），已正名为 GetLogicalShareSizeIncludingDeleted；
// 若有人重新引入旧名，语义会再次与 GetPhysicalObjectUsage 混淆。
func TestGetTotalSizeIncludingDeleted_MustNotRevive(t *testing.T) {
	repo := NewFileCodeRepository(newGovernanceTestDB(t))
	typ := reflect.TypeOf(repo)
	_, ok := typ.MethodByName("GetTotalSizeIncludingDeleted")
	assert.Falsef(t, ok,
		"GetTotalSizeIncludingDeleted 已改名（core#193），不得复活——用 GetLogicalShareSizeIncludingDeleted / GetPhysicalObjectUsage")
	_, ok = typ.MethodByName("GetLogicalShareSizeIncludingDeleted")
	assert.True(t, ok, "正名后的逻辑口径方法必须存在")
	_, ok = typ.MethodByName("GetPhysicalObjectUsage")
	assert.True(t, ok, "物理口径方法必须存在")
}
