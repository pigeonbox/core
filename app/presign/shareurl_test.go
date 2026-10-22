package presign

import (
	"context"
	"testing"

	"github.com/pigeonbox/core/pkg/baseurl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 匿名取件码绑定通道的链接形态契约（2026-10-23 跨仓审计）。
//
// 缺陷实录：createShareRecord 在「取件码绑定」分支把 **API 路径**
// "/share/select/?code=XXX" 当页面链接返回（shareURL），且 fullURL 返空。
// 连锁伤害两条：
//  1. 前端拿 share_url 做兜底转换 ⇒ origin/#/share/select/?code=XXX
//     ⇒ vue-router 命中 /share/:code（code="select"）⇒ 打开就是「分享不存在」；
//  2. presign complete 用 SplitSharePageURL 从该 URL 剥 base —— 两个候选
//     后缀都不匹配 ⇒ 原样当 base ⇒ download_url 拼成
//     "/share/select/?code=XXX/share/download?code=…&token=…" 垃圾串。
//
// 判据：页面链接必须是 hash 路由形态 {base}/#/share/{分享码}，
// 且能从中正确剥出 base 供直链派生。

type mockPickupBinder struct {
	shareCode string
	bound     bool
}

func (m *mockPickupBinder) ResolvePlaceholder(ctx context.Context, pickupCode string) (string, error) {
	return m.shareCode, nil
}

func (m *mockPickupBinder) BindFilePath(ctx context.Context, shareCode, filePath string, size int64, note string) error {
	m.bound = true
	return nil
}

func TestPickupBinderBranch_ShareLinkIsHashForm(t *testing.T) {
	svc, _ := newTestService(t)
	binder := &mockPickupBinder{shareCode: "AbCdEfGh"}
	svc.SetPickupBinder(binder)

	code, shareURL, fullURL, pickupCode, err := svc.createShareRecord(context.Background(), &InitMeta{
		PickupCode: "KJ8Y2M",
		ObjectKey:  "obj/x.bin",
		FileSize:   123,
	}, "1.2.3.4")
	require.NoError(t, err)

	assert.Equal(t, "AbCdEfGh", code)
	assert.Equal(t, "KJ8Y2M", pickupCode)
	assert.True(t, binder.bound)

	// 页面链接 = hash 路由形态（不是 API 路径）
	assert.Contains(t, shareURL, "/#/share/AbCdEfGh")
	assert.NotContains(t, shareURL, "/share/select/")
	assert.Contains(t, fullURL, "/#/share/AbCdEfGh")
	assert.NotContains(t, fullURL, "/share/select/")

	// 直链派生：能从页面链接剥出 base ⇒ download_url 不是垃圾串
	base := baseurl.SplitSharePageURL(fullURL, code)
	assert.Equal(t, "http://test.local", base)
	assert.Equal(t, "http://test.local/share/download?code=AbCdEfGh&token=TK",
		baseurl.ShareDownloadURL(base, code, "TK"))
}
