package admin

import (
	"context"
	"testing"
	"time"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ==================== 取件失败事件留存（frontend-core#93 拍板：7 天） ====================

// TestCleanupPickupFailures_RetentionAndOrphan 三段判据：
// 超 7 天删、窗口内留、分享不存活（孤儿）删；并钉死 7 天这个拍板值。
func TestCleanupPickupFailures_RetentionAndOrphan(t *testing.T) {
	_, svc, g := newJanitorTestEnv(t)
	require.NoError(t, g.AutoMigrate(&model.PickupFailure{}))
	ctx := context.Background()

	// 拍板值钉死：失败事件固定 7 天（与分享过期语义对齐）
	assert.Equal(t, 7, PickupFailureRetentionDays, "失败事件留存 7 天是拍板值，不得漂移")

	// 存活分享（时间线的合法归属）
	require.NoError(t, g.Create(&model.FileCode{Code: "ALIVE001", Text: "x"}).Error)

	now := time.Now()
	oldAt := now.AddDate(0, 0, -8)  // 超 7 天 ⇒ 删
	recentAt := now.Add(-time.Hour) // 窗口内 ⇒ 留
	insert := func(code, reason string, at time.Time) {
		require.NoError(t, g.Create(&model.PickupFailure{
			Code: code, Reason: reason, SourceHash: "hash-" + reason, FailedAt: at,
		}).Error)
	}
	insert("ALIVE001", model.PickupFailExpired, oldAt)           // ① 超龄 ⇒ 删
	insert("ALIVE001", model.PickupFailPasswordWrong, recentAt)  // ② 窗口内 ⇒ 留
	insert("GONE0001", model.PickupFailCountExhausted, recentAt) // ③ 孤儿 ⇒ 删

	j := NewJanitor(g, svc, 90)
	n, err := j.CleanupPickupFailures(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 2, n, "超龄 1 + 孤儿 1 = 2")

	var rows []model.PickupFailure
	require.NoError(t, g.Where("1 = 1").Find(&rows).Error)
	require.Len(t, rows, 1, "只应剩窗口内的存活分享事件")
	assert.Equal(t, "ALIVE001", rows[0].Code)
	assert.Equal(t, model.PickupFailPasswordWrong, rows[0].Reason)
}
