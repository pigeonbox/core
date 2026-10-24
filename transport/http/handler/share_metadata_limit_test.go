// /share/metadata 面的「次数耗尽」错误码回归（2026-10-24）
//
// 口径：次数耗尽 ⇒ 20004「超过取件次数」（CodeShareReachLimit），**不再**与
// 「不存在/已过期」一起塌成 404（errcode.CodeNotFound + 「分享不存在或已过期」）。
//
// 理由：404 是**笼统的存在性模糊**（防探测面：码不存在/已过期/被封禁统一
// 404）；而「次数耗尽」是一个**可验证的事实**（持码者确实拿到了分享，只是
// 额度用完）——伪装成 404 会让持码者以为码写错了，而真相是「来晚了」。
// 且本端点对「已过期」也回 404（旧口径），耗尽单独给码不扩大探测面：
// 攻击者仍需先猜中 6 位码才能看到 20004。
//
// 断言必须能变红：把 handler 里 ErrShareCountExhausted 分支删掉（让它落回
// 兜底 404），本用例即失败。
package handler

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/app/share"
	"github.com/pigeonbox/core/pkg/utils"
	"github.com/pigeonbox/core/repo/db/model"
)

// newMetadataHandlerSvc 构造只够跑 metadata 面的 share service（内存 sqlite）。
func newMetadataHandlerSvc(t *testing.T) (*share.Service, *gorm.DB) {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}, &model.FileCodeFile{}, &model.PickupCharge{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return share.NewService(g, "http://localhost:12345", nil), g
}

// TestShareMetadata_CountExhaustedReturns20004 额度用完 ⇒ 403 + 20004。
func TestShareMetadata_CountExhaustedReturns20004(t *testing.T) {
	svc, g := newMetadataHandlerSvc(t)
	SetShareService(svc)
	t.Cleanup(func() { SetShareService(nil) })

	ctx := context.Background()
	resp, err := svc.CreateShare(ctx, &share.ShareFileReq{
		Channel:      "direct",
		FilePath:     "uploads/2026/10/24/meta.txt",
		Size:         4,
		Text:         "meta.txt",
		ExpiredAt:    utils.CalculateExpireTime(1, "day"),
		ExpiredCount: utils.CalculateExpireCount("count", 1),
		UploadType:   "authenticated",
	})
	require.NoError(t, err)

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.GET("/share/metadata/:code", ShareMetadata)

	// 额度未用完 ⇒ 200 + 正常元数据（正向对照：不是「永远拒」）
	w := ut.PerformRequest(h.Engine, "GET", "/share/metadata/"+resp.Code, nil,
		ut.Header{Key: "Host", Value: "test.local"})
	require.Equal(t, 200, w.Code, "额度未用完时应返回 200：%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "meta.txt")

	// 额度用完 ⇒ 403 + 20004
	require.NoError(t, g.Model(&model.FileCode{}).Where("code = ?", resp.Code).
		Update("expired_count", 0).Error)

	w = ut.PerformRequest(h.Engine, "GET", "/share/metadata/"+resp.Code, nil,
		ut.Header{Key: "Host", Value: "test.local"})

	var body struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, 403, w.Code, "额度用完应回 403：%s", w.Body.String())
	assert.Equal(t, 20004, body.Code, "额度用完应是 20004「超过取件次数」，不得塌成 404：%s", w.Body.String())
	assert.NotContains(t, w.Body.String(), "meta.txt", "额度用完不得回显文件名")
}
