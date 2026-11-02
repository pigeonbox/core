// transfer.max_count 在匿名占位通道的收口（2026-11-02 接线）。
//
// CreateAnonymousShare 直建 file_codes 记录、不走 share 域漏斗
// （createWithCode/createWithRetry）⇒ 必须在此单独收口，否则本通道是
// 「单分享可取件次数上限」的旁路（presign 取件码绑定正是回填到这里的占位记录）。
package anonymous

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/conf"
)

// TestCreateAnonymousShare_PickupCountCap 超限显式拒绝 + 恰等上限放行（负对照）。
func TestCreateAnonymousShare_PickupCountCap(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	old := conf.GetGlobalConfig()
	conf.SetGlobalConfig(&conf.AppConfiguration{Transfer: conf.TransferConfig{MaxCount: 2, ExpireDefault: 7}})
	t.Cleanup(func() { conf.SetGlobalConfig(old) })

	expire := time.Now().Add(time.Hour)
	base := AnonymousShareParams{
		FilePath: "uploads/2026/11/02/cap.bin",
		FileName: "cap.bin",
		FileSize: 8,
		ExpireAt: &expire,
	}

	// 超限（3 > 2）⇒ 拒
	p := base
	p.MaxPickupCount = 3
	_, err := svc.CreateAnonymousShare(ctx, p)
	require.Error(t, err, "匿名占位通道必须拒超限取件次数")
	assert.Contains(t, err.Error(), "取件次数超过站点上限")

	// 负对照：恰等上限放行
	p = base
	p.MaxPickupCount = 2
	code, err := svc.CreateAnonymousShare(ctx, p)
	require.NoError(t, err, "恰等上限必须放行")
	assert.Len(t, code, codeLength)

	// 负对照：0/-1 = 无限 ⇒ 不受上限约束（不得被误拒）
	p = base
	p.MaxPickupCount = 0
	_, err = svc.CreateAnonymousShare(ctx, p)
	require.NoError(t, err, "0=默认无限，不得被上限误拒")
}

// TestCreateAnonymousShare_CapZeroMeansUnlimited 上限 0=显式不限。
func TestCreateAnonymousShare_CapZeroMeansUnlimited(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	old := conf.GetGlobalConfig()
	conf.SetGlobalConfig(&conf.AppConfiguration{Transfer: conf.TransferConfig{MaxCount: 0, ExpireDefault: 7}})
	t.Cleanup(func() { conf.SetGlobalConfig(old) })

	expire := time.Now().Add(time.Hour)
	_, err := svc.CreateAnonymousShare(ctx, AnonymousShareParams{
		FilePath: "uploads/2026/11/02/cap3.bin", FileName: "cap3.bin", FileSize: 8,
		ExpireAt: &expire, MaxPickupCount: 1000,
	})
	require.NoError(t, err, "上限 0=显式不限")
}
