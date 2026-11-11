// core#139 判据：app/admin EnhancedStats 在坏库下必须**显式失败**，不再单项
// 静默降级 0（坏库下 200 全 0 是假数据且无错误态，与 core#102「失败必须可见」
// 相悖；TAIL2 core#137 已同口径收口 GetStats/GetConfig）。
//
// TrendSeries 的单项降级是文档化兼容决策，不在本判据射程。
//
// 负对照（实做）：任一查询回退 `stats.X, _ = …` 吞错 ⇒ 「坏库必须报错」判据转红。
package admin

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/model"
)

func TestEnhancedStats_BrokenDBFailsVisible(t *testing.T) {
	ctx := context.Background()

	t.Run("坏库 ⇒ 显式错误（修前 200 全 0）", func(t *testing.T) {
		stats, err := NewService(newBrokenTestDB(t)).EnhancedStats(ctx)
		assert.Error(t, err, "坏库下 EnhancedStats 必须报错，不得静默降级 0")
		assert.Nil(t, stats, "失败不得给半成品统计")
	})

	t.Run("健康库 ⇒ 真实数值（成功路径零变化）", func(t *testing.T) {
		g := newAdminTestDB(t)
		svc := NewService(g)
		require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
			Code: "ENHS001", Text: "a.pdf", FilePath: "uploads/a.pdf",
			Size: 7, UsedCount: 5, ExpiredCount: -1, UploadType: "anonymous",
		}))
		require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
			Code: "ENHS002", Text: "b.png", FilePath: "uploads/b.png",
			Size: 3, UsedCount: 2, ExpiredCount: -1, UploadType: "presign_anonymous",
		}))

		stats, err := svc.EnhancedStats(ctx)
		require.NoError(t, err)
		require.NotNil(t, stats)
		assert.Equal(t, int64(2), stats.TodayUploads, "刚建的两行计入今日")
		assert.Equal(t, int64(0), stats.YesterdayUploads)
		assert.Equal(t, int64(7), stats.TotalDownloads, "SUM(used_count)")
		assert.Equal(t, int64(1), stats.AnonymousFiles)
		assert.Equal(t, int64(1), stats.PresignFiles, "presign_anonymous + presign_authenticated")
		assert.Equal(t, int64(10), stats.StorageUsed, "SUM(size)")
		require.Len(t, stats.TopSuffixes, 2, "类型分布来自现役列（text 原始名）")
	})
}
