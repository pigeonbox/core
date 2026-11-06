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
