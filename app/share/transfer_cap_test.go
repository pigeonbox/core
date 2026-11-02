// transfer.max_count「单分享可取件次数上限」漏斗守卫（2026-11-02 接线）。
//
// 上限在 createWithCode/createWithRetry 两条写库漏斗收口（不静默钳制、超限
// 显式拒绝），覆盖一切经本域建的分享：文本 / 文件 / 多文件。
// 断言打在**每个通道**上——漏一个通道就是静默放行。
package share

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/conf"
)

// TestPickupCountCapOnAllChannels 超限显式拒绝 + 恰等上限放行（负对照）。
func TestPickupCountCapOnAllChannels(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()
	old := conf.GetGlobalConfig()
	conf.SetGlobalConfig(&conf.AppConfiguration{Transfer: conf.TransferConfig{MaxCount: 2, ExpireDefault: 7}})
	t.Cleanup(func() { conf.SetGlobalConfig(old) })

	// 超限（3 > 2）：三个通道都必须拒
	_, err := svc.ShareText(ctx, &ShareTextReq{Text: "x", ExpiredCount: 3})
	require.Error(t, err, "文本通道必须拒超限取件次数")
	assert.Contains(t, err.Error(), "取件次数超过站点上限")

	_, err = svc.ShareText(ctx, &ShareTextReq{Text: "x", ExpiredCount: 3, CustomCode: "cap-test-1"})
	require.Error(t, err, "自定义码路径（createWithCode）同样必须拒")

	_, err = svc.CreateShare(ctx, &ShareFileReq{
		Channel: "direct", FilePath: "uploads/2026/11/02/cap.bin", Size: 8, Text: "cap.bin",
		ExpiredCount: 3,
	})
	require.Error(t, err, "文件通道必须拒超限取件次数")

	_, err = svc.CreateMultiFileShare(ctx, &MultiShareReq{
		Entries:      []StoredFileEntry{{RelPath: "uploads/2026/11/02/cap2.bin", Size: 4}},
		ExpiredCount: 3,
	})
	require.Error(t, err, "多文件通道必须拒超限取件次数")

	// 负对照：恰等上限放行（否则"拒"可能是一律拒的假红）
	_, err = svc.ShareText(ctx, &ShareTextReq{Text: "x", ExpiredCount: 2})
	require.NoError(t, err, "恰等上限必须放行")

	// 负对照：不按次数过期（-1）不受上限约束
	_, err = svc.ShareText(ctx, &ShareTextReq{Text: "y", ExpiredCount: -1})
	require.NoError(t, err)
}

// TestPickupCountCapZeroMeansUnlimited 上限 0 = 显式不限（不得当"未配置"）。
func TestPickupCountCapZeroMeansUnlimited(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()
	old := conf.GetGlobalConfig()
	conf.SetGlobalConfig(&conf.AppConfiguration{Transfer: conf.TransferConfig{MaxCount: 0, ExpireDefault: 7}})
	t.Cleanup(func() { conf.SetGlobalConfig(old) })

	_, err := svc.ShareText(ctx, &ShareTextReq{Text: "x", ExpiredCount: 1000})
	require.NoError(t, err, "上限 0=显式不限，任意次数必须放行")
}
