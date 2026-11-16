// core#220：次数型分享 expire_value 下界守卫。
//
// 现状（修复前）：
//   - CheckPickupCountCap 对 n<=0 一律放行（只挡超上限）；
//   - CalculateExpireCount("count", v) 原样返回 v；
//   - handler 只校验样式白名单，不校验数值。
//
// ⇒ count+0 落库 ExpiredCount=0（建完即死，janitor 当成过期行）；
//
//	count+负数 落库负值（IsCountExhausted 只判 ==0 ⇒ 负数变成无限次）。
//
// 本文件钉住修复后的判据：
//
//	① count 型必须 1 <= value <= transfer.max_count，0/负数显式拒绝；
//	② CheckPickupCountCap 只允许 -1（无限）与非负正数；0 与 <-1 拒绝；
//	③ CalculateExpireCount 对 count<=0 的兜底归一为 -1（入口已拒绝；兜底保证
//	   任何漏校验的新调用方也不会写出 0/负值）。
package utils

import (
	"testing"

	"github.com/pigeonbox/core/conf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckCountExpireValue(t *testing.T) {
	withTransferConfig(t, conf.TransferConfig{MaxCount: 10, ExpireDefault: 7})

	t.Run("count 0 拒绝", func(t *testing.T) {
		err := CheckCountExpireValue("count", 0)
		require.Error(t, err, "count+0 会建出「建完即死」的分享，必须在入口拒绝")
		assert.Contains(t, err.Error(), "正整数")
	})
	t.Run("count 负数拒绝", func(t *testing.T) {
		for _, v := range []int{-1, -5, -100} {
			err := CheckCountExpireValue("count", v)
			require.Errorf(t, err, "count+%d 会把次数限制变成无限次，必须在入口拒绝", v)
			assert.Contains(t, err.Error(), "正整数")
		}
	})
	t.Run("count 超上限拒绝", func(t *testing.T) {
		err := CheckCountExpireValue("count", 11)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "取件次数超过站点上限")
	})
	t.Run("count 1..上限 放行", func(t *testing.T) {
		for _, v := range []int{1, 10} {
			require.NoErrorf(t, CheckCountExpireValue("count", v), "count+%d 应放行", v)
		}
	})
	t.Run("非 count 样式不做次数校验", func(t *testing.T) {
		for _, style := range []string{"", "day", "forever", "hour"} {
			for _, v := range []int{0, -1, -5, 1000} {
				require.NoErrorf(t, CheckCountExpireValue(style, v),
					"样式 %q 的值 %d 不是次数语义，不应被次数下界拦截", style, v)
			}
		}
	})
	t.Run("大小写与空白归一", func(t *testing.T) {
		require.Error(t, CheckCountExpireValue("COUNT", 0))
		require.Error(t, CheckCountExpireValue(" count ", 0))
		require.NoError(t, CheckCountExpireValue("COUNT", 3))
	})
}

// TestCheckPickupCountCapRejectsInvalidStoredCount 落库漏斗的取值域守卫：
// -1=无限（唯一允许的非正值）；0=已耗尽、<-1=历史脏值，都不允许作为新建值。
func TestCheckPickupCountCapRejectsInvalidStoredCount(t *testing.T) {
	withTransferConfig(t, conf.TransferConfig{MaxCount: 3, ExpireDefault: 7})

	assert.NoError(t, CheckPickupCountCap(-1), "-1 = 无限，必须放行")
	for _, v := range []int{0, -2, -100} {
		require.Errorf(t, CheckPickupCountCap(v), "%d 不是合法的新建次数（-1=无限，>0=剩余）", v)
	}
	assert.NoError(t, CheckPickupCountCap(3))
	require.Error(t, CheckPickupCountCap(4))
}

// TestCalculateExpireCountFallback count<=0 不再原样落库（兜底归一为 -1）。
func TestCalculateExpireCountFallback(t *testing.T) {
	assert.Equal(t, 5, CalculateExpireCount("count", 5))
	for _, v := range []int{0, -1, -5} {
		assert.Equal(t, -1, CalculateExpireCount("count", v),
			"count+%d 必须归一为 -1（入口已拒绝，兜底防漏网写出 0/负值）", v)
	}
	assert.Equal(t, -1, CalculateExpireCount("day", 5))
}
