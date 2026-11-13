// 「我的分享」列表 status 参数的**枚举严格校验**守卫（core#149，#102 同族）。
//
// 缺陷形态：userShareQuerySpec 的 switch 只有已知分支、**没有 default** ⇒
// 未知 status（拼写错/大小写错）⇒ 状态条件根本不落 ⇒ 返回**未筛选的全量**
// （活跃/过期/文本/文件混在一起），而调用方以为筛过了。
// 「筛选失败」比「筛选为空」危险：前者**多给数据**（#102 的原话）。
//
// 判据：未知 status ⇒ error（handler 统一 400 + 10001 + 点名参数与合法取值）；
// 已知取值与空串/"all" 照旧放行。
// 负对照：把校验分支删掉 ⇒ 本文件转红。
package share

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/model"
)

// userPtr 测试用 userID 指针。
func userPtr(v uint) *uint { return &v }

// expiredAtPast 测试用「已过期」时间。
func expiredAtPast() *time.Time { p := time.Now().Add(-time.Hour); return &p }

func TestListUserShares_UnknownStatusRejected(t *testing.T) {
	svc, g, _, _, _ := newTestServiceWithDB(t)
	ctx := context.Background()

	// 两条不同状态的分享：未知 status 若静默丢条件，会把两条都返回。
	require.NoError(t, g.Create(&model.FileCode{
		Code: "STACT01", UserID: userPtr(1), ExpiredCount: -1,
		ExpiredAt: nil, // 永不过期 ⇒ active
	}).Error)
	require.NoError(t, g.Create(&model.FileCode{
		Code: "STEXPI1", UserID: userPtr(1), ExpiredCount: -1,
		ExpiredAt: expiredAtPast(), // 已过期 ⇒ expired
	}).Error)

	// 合法取值照旧工作（收紧不得误伤）
	items, total, err := svc.ListUserShares(ctx, 1, "all", "", 1, 20)
	require.NoError(t, err, "status=all 必须放行")
	require.EqualValues(t, 2, total, "all 应返回两条")
	require.Len(t, items, 2)

	items, total, err = svc.ListUserShares(ctx, 1, "active", "", 1, 20)
	require.NoError(t, err, "status=active 必须放行")
	require.EqualValues(t, 1, total)
	require.Len(t, items, 1)
	assert.Equal(t, "STACT01", items[0].Code)

	// 未知取值：显式失败，而不是「当没筛」
	_, _, err = svc.ListUserShares(ctx, 1, "bogus", "", 1, 20)
	require.Error(t, err, "未知 status 必须报错（不得静默丢条件返回全量）")
	assert.Contains(t, err.Error(), "status", "错误必须点名参数：%v", err)

	_, _, err = svc.ListUserShares(ctx, 1, "Active", "", 1, 20)
	require.Error(t, err, "大小写变体同样不得静默当全量：%v", err)

	// 空串 = 不筛（历史口径不动）
	_, total, err = svc.ListUserShares(ctx, 1, "", "", 1, 20)
	require.NoError(t, err, "空 status 必须放行（= 不筛）")
	require.EqualValues(t, 2, total)
}
