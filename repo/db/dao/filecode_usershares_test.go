package dao

import (
	"context"
	"testing"
	"time"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func uintPtr(v uint) *uint { return &v }

// 守卫测试：我的分享列表类型分类。
// 写侧惯例：文件分享的原始文件名存 Text 字段（uuid_file_name 常为空），
// 因此 text/file 分类必须按 file_path 判定，Text != "" 不能作为文本分享依据——
// 否则文件分享被误归文本（「文件」tab 恒空回归，2026-10-06 实测）。
func TestGetUserSharesWithFilterTypeClassification(t *testing.T) {
	g := newGovernanceTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	fileShare := &model.FileCode{
		Code:     "FILE0001",
		FilePath: "uploads/2026/10/06/uuid-name.txt",
		Text:     "原始文件.txt",
		Size:     32,
		UserID:   uintPtr(1),
	}
	textShare := &model.FileCode{
		Code:   "TEXT0001",
		Text:   "一段普通的文本内容",
		UserID: uintPtr(1),
	}
	require.NoError(t, repo.Create(ctx, fileShare))
	require.NoError(t, repo.Create(ctx, textShare))

	t.Run("file 过滤按 file_path 判定,文件分享可见", func(t *testing.T) {
		items, total, err := repo.GetUserSharesWithFilter(ctx, 1, UserShareFilter{Status: "file", Page: 1, PageSize: 20})
		require.NoError(t, err)
		assert.Equal(t, int64(1), total)
		require.Len(t, items, 1)
		assert.Equal(t, "FILE0001", items[0].Code)
	})

	t.Run("text 过滤不含文件分享", func(t *testing.T) {
		items, total, err := repo.GetUserSharesWithFilter(ctx, 1, UserShareFilter{Status: "text", Page: 1, PageSize: 20})
		require.NoError(t, err)
		assert.Equal(t, int64(1), total)
		require.Len(t, items, 1)
		assert.Equal(t, "TEXT0001", items[0].Code)
	})

	t.Run("搜索按原始文件名命中文件分享", func(t *testing.T) {
		items, total, err := repo.GetUserSharesWithFilter(ctx, 1, UserShareFilter{Status: "all", Search: "原始文件", Page: 1, PageSize: 20})
		require.NoError(t, err)
		assert.Equal(t, int64(1), total)
		require.Len(t, items, 1)
		assert.Equal(t, "FILE0001", items[0].Code)
	})
}

// 守卫测试：status=viewed（取件历史页数据源）只含被取件过的分享。
// 此前前端拉 all 全量再客户端过滤 viewer_at，分页 total 是全量分享数，
// 出现「表格空态却显示共 N 条」（2026-10-08 用户报告）；后端补 viewed
// 过滤后 total 与列表同源。
func TestGetUserSharesWithFilterViewed(t *testing.T) {
	g := newGovernanceTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	viewedAt := time.Now()
	picked := &model.FileCode{
		Code:        "PICKED01",
		Text:        "被取件过的文本分享",
		UserID:      uintPtr(1),
		ViewerIP:    "10.0.0.9",
		ViewerAt:    &viewedAt,
		ViewerCount: 1,
	}
	unpicked := &model.FileCode{
		Code:   "FRESH001",
		Text:   "从未被取件的文本分享",
		UserID: uintPtr(1),
	}
	other := &model.FileCode{
		Code:     "OTHERV01",
		Text:     "别人分享的被取件文本",
		UserID:   uintPtr(2),
		ViewerIP: "10.0.0.9",
		ViewerAt: &viewedAt,
	}
	require.NoError(t, repo.Create(ctx, picked))
	require.NoError(t, repo.Create(ctx, unpicked))
	require.NoError(t, repo.Create(ctx, other))

	items, total, err := repo.GetUserSharesWithFilter(ctx, 1, UserShareFilter{Status: "viewed", Page: 1, PageSize: 20})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total, "viewed 只计本用户被取件过的分享")
	require.Len(t, items, 1)
	assert.Equal(t, "PICKED01", items[0].Code)

	allItems, allTotal, err := repo.GetUserSharesWithFilter(ctx, 1, UserShareFilter{Status: "all", Page: 1, PageSize: 20})
	require.NoError(t, err)
	assert.Equal(t, int64(2), allTotal)
	assert.Len(t, allItems, 2)
}

// 守卫测试：status=expired（过期 tab）＝时间已到 或 次数耗尽，与 active 互补。
// 此前 expired 分支 Where 带 `expired_at <= ?` 占位符却没传 now 参数，
// GORM 构建报 missing argument with index 2，「过期」tab 从上线起恒 500
// （2026-10-16 用户报告；legacy 单体时代同款漏参，历次重构原样搬运）。
func TestGetUserSharesWithFilterExpired(t *testing.T) {
	g := newGovernanceTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(24 * time.Hour)
	expiredByTime := &model.FileCode{
		Code:         "TIMEOUT1",
		Text:         "时间已过的文本分享",
		UserID:       uintPtr(1),
		ExpiredAt:    &past,
		ExpiredCount: -1,
	}
	exhaustedByCount := &model.FileCode{
		Code:         "USEDUP01",
		Text:         "次数耗尽的文本分享",
		UserID:       uintPtr(1),
		ExpiredCount: 0,
	}
	active := &model.FileCode{
		Code:         "ACTIV001",
		Text:         "仍在有效期的文本分享",
		UserID:       uintPtr(1),
		ExpiredAt:    &future,
		ExpiredCount: 5,
	}
	neverExpires := &model.FileCode{
		Code:         "NEVERSE1",
		Text:         "永不过期的文本分享",
		UserID:       uintPtr(1),
		ExpiredCount: -1,
	}
	other := &model.FileCode{
		Code:         "OTHEREX1",
		Text:         "别人的过期分享不串列",
		UserID:       uintPtr(2),
		ExpiredAt:    &past,
		ExpiredCount: -1,
	}
	require.NoError(t, repo.Create(ctx, expiredByTime))
	require.NoError(t, repo.Create(ctx, exhaustedByCount))
	require.NoError(t, repo.Create(ctx, active))
	require.NoError(t, repo.Create(ctx, neverExpires))
	require.NoError(t, repo.Create(ctx, other))

	items, total, err := repo.GetUserSharesWithFilter(ctx, 1, UserShareFilter{Status: "expired", Page: 1, PageSize: 20})
	require.NoError(t, err)
	assert.Equal(t, int64(2), total, "expired 只计本用户时间已过/次数耗尽的分享")
	require.Len(t, items, 2)
	got := map[string]bool{items[0].Code: true, items[1].Code: true}
	assert.True(t, got["TIMEOUT1"], "时间已过的分享应在 expired 列表")
	assert.True(t, got["USEDUP01"], "次数耗尽的分享应在 expired 列表")

	// 与 active 互补：active 恰好是其余两条本用户分享
	activeItems, activeTotal, err := repo.GetUserSharesWithFilter(ctx, 1, UserShareFilter{Status: "active", Page: 1, PageSize: 20})
	require.NoError(t, err)
	assert.Equal(t, int64(2), activeTotal)
	gotActive := map[string]bool{activeItems[0].Code: true, activeItems[1].Code: true}
	assert.True(t, gotActive["ACTIV001"])
	assert.True(t, gotActive["NEVERSE1"])
}
