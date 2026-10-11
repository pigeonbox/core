package dao

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/model"
)

// TestCountByHealth_And_ListFilter 文件健康洞察口径（2026-10-07）：
// CountByHealth 与 ListWithFilter(Health) 必须同源（applyHealthFilter 单点），
// 此测试锁两接口的数字一致性——仪表盘卡与文件管理页过滤视图不能各说各话。
func TestCountByHealth_And_ListFilter(t *testing.T) {
	g := newGovernanceTestDB(t)
	repo := NewFileCodeRepository(g)
	ctx := context.Background()

	now := time.Now()
	mk := func(code string, expiredAt *time.Time, expiredCount int, usedCount int) {
		require.NoError(t, repo.Create(ctx, &model.FileCode{
			Code:         code,
			FilePath:     "uploads/x/" + code + ".bin",
			ExpiredAt:    expiredAt,
			ExpiredCount: expiredCount,
			UsedCount:    usedCount,
		}))
	}

	// 可取件且未取件（1 小时后过期）
	mk("ACT00001", ptrTime(now.Add(time.Hour)), -1, 0)
	// 可取件且已取件（2 小时后过期）
	mk("ACT00002", ptrTime(now.Add(2*time.Hour)), -1, 1)
	// 即将过期且未取件（30 分钟后过期）
	mk("SOON0001", ptrTime(now.Add(30*time.Minute)), -1, 0)
	// 永久有效
	mk("FRV00001", nil, -1, 0)
	// 永久但已被取件（不计入 never_picked，计入 forever+active）
	mk("FRV00002", nil, -1, 2)
	// 已过期（时间式）
	mk("EXP00001", ptrTime(now.Add(-time.Hour)), -1, 0)
	// 已过期（次数耗尽）
	mk("EXP00002", nil, 0, 1)

	active, expired, expiringSoon, neverPicked, forever, err := repo.CountByHealth(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(5), active, "可取件=3 未过期时间式+2 永久")
	assert.Equal(t, int64(2), expired, "已过期=时间式+次数耗尽")
	assert.Equal(t, int64(3), expiringSoon, "24h 内到期=1h/2h/30min 三条全部落在窗口内")
	assert.Equal(t, int64(3), neverPicked, "未取件=ACT00001+SOON0001+FRV00001（永久未取件同样计入）")
	assert.Equal(t, int64(2), forever, "永久=两条")

	// ListWithFilter(Health) 与 CountByHealth 同口径
	cases := map[string]int64{
		"active":        5,
		"expired":       2,
		"expiring_soon": 3,
		"never_picked":  3,
		"forever":       2,
	}
	for health, want := range cases {
		_, total, err := repo.ListWithFilter(ctx, model.FileCodeQuery{Health: health, Page: 1, PageSize: 50})
		require.NoError(t, err, health)
		assert.Equal(t, want, total, "health=%s", health)
	}

	// 未知 health 值 = 不过滤（防御：不 panic 不误杀）
	_, total, err := repo.ListWithFilter(ctx, model.FileCodeQuery{Health: "hacked", Page: 1, PageSize: 50})
	require.NoError(t, err)
	assert.Equal(t, int64(7), total)
}

func ptrTime(t time.Time) *time.Time { return &t }
