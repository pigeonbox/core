// 取件失败事件记账点测试（frontend-core#93：select / download 面）。
//
// 复现口径：三类可归因失败（密码错 / 次数耗尽 / 过期）必须各落一条事件，
// 且**码不存在不落**（不可归因 + 枚举流量不开 DB 写放大面）——断言都能变红：
// 抽掉 handler 里任一 RecordPickupFailure 调用，对应用例立刻红。
package share

import (
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/pkg/utils"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
)

// newFailureEventEnv 构造带 pickup_failures 表的 handler 测试环境。
// ⚠️ 不改 newChargeTestEnv 的 AutoMigrate 清单（并行在途会话的测试面）——
// 本文件自己补迁移，两个会话互不触碰。
func newFailureEventEnv(t *testing.T, tokenEnabled bool) *gorm.DB {
	t.Helper()
	g := newChargeTestEnv(t, tokenEnabled)
	require.NoError(t, g.AutoMigrate(&model.PickupFailure{}))
	return g
}

// performGetHeaders 可带多 header 的 select 请求（密码经 X-Share-Password 头）。
func performGetHeaders(t *testing.T, query string, headers ...ut.Header) (int, string) {
	t.Helper()
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.GET("/share/select/", GetShare)
	w := ut.PerformRequest(h.Engine, "GET", "/share/select/?"+query, nil, headers...)
	return w.Code, w.Body.String()
}

// failureEventRows 读某分享的失败事件（落库序）。
func failureEventRows(t *testing.T, g *gorm.DB, code string) []model.PickupFailure {
	t.Helper()
	var rows []model.PickupFailure
	require.NoError(t, g.Where("code = ?", code).Order("id").Find(&rows).Error)
	return rows
}

// TestGetShare_PasswordWrong_RecordsPickupFailureEvent 密码错误（select 面）落事件；
// 同时钉住计次不变量：失败不消耗次数。
func TestGetShare_PasswordWrong_RecordsPickupFailureEvent(t *testing.T) {
	g := newFailureEventEnv(t, true)
	ctx := t.Context()
	hash, err := utils.HashPassword("rightpwd")
	require.NoError(t, err)
	repo := dao.NewFileCodeRepository(g)
	require.NoError(t, repo.Create(ctx, &model.FileCode{
		Code: "PWFAIL01", Text: "x", FilePath: "uploads", UUIDFileName: "x.bin",
		Size: 1, ExpiredCount: 3, RequireAuth: true, PasswordHash: hash,
	}))

	status, body := performGetHeaders(t, "code=PWFAIL01",
		ut.Header{Key: "Host", Value: "test.local"},
		ut.Header{Key: "X-Share-Password", Value: "wrongpwd"})
	require.Contains(t, body, "密码错误", "密码错误语义不得变：%d %s", status, body)
	require.NotEqual(t, 200, status, "错密码不得交付内容")

	rows := failureEventRows(t, g, "PWFAIL01")
	require.Len(t, rows, 1, "密码错误必须落一条失败事件")
	assert.Equal(t, model.PickupFailPasswordWrong, rows[0].Reason)
	assert.Equal(t, "select", rows[0].Channel)
	assert.NotEmpty(t, rows[0].SourceHash, "来源指纹应非空（ut 环境有解析 IP）")
	assert.NotEqual(t, "0.0.0.0", rows[0].SourceHash, "指纹不是 IP 的任何形态")

	// 计次不变量：失败只记录，次数一分不动
	fc, err := repo.GetByCode(ctx, "PWFAIL01")
	require.NoError(t, err)
	assert.Equal(t, 3, fc.ExpiredCount, "失败事件不得消耗取件次数（17 条不变量）")
	assert.Equal(t, 0, fc.UsedCount)
}

// TestGetShare_CountExhausted_RecordsPickupFailureEvent 次数耗尽（select 面）
// 落事件，原因与 wire 错误码 20004 对齐。
func TestGetShare_CountExhausted_RecordsPickupFailureEvent(t *testing.T) {
	g := newFailureEventEnv(t, true)
	ctx := t.Context()
	repo := dao.NewFileCodeRepository(g)
	require.NoError(t, repo.Create(ctx, &model.FileCode{
		Code: "EXHFAIL01", Text: "x", FilePath: "uploads", UUIDFileName: "x.bin",
		Size: 1, ExpiredCount: 0, // 次数耗尽（不是时间过期）
	}))

	status, body := performGet(t, "/share/select/", "code=EXHFAIL01",
		ut.Header{Key: "Host", Value: "test.local"})
	require.Equal(t, 403, status, "次数耗尽应 403（20004）：%s", body)
	require.Contains(t, body, "超过取件次数")

	rows := failureEventRows(t, g, "EXHFAIL01")
	require.Len(t, rows, 1, "次数耗尽必须落一条失败事件")
	assert.Equal(t, model.PickupFailCountExhausted, rows[0].Reason)
	assert.Equal(t, "select", rows[0].Channel)
}

// TestGetShare_Expired_RecordsPickupFailureEvent 过期（select 面）落事件，
// 且归因到**规范分享码**（6 位取件码入口也一样）。
func TestGetShare_Expired_RecordsPickupFailureEvent(t *testing.T) {
	g := newFailureEventEnv(t, true)
	ctx := t.Context()
	past := time.Now().Add(-time.Hour)
	repo := dao.NewFileCodeRepository(g)
	require.NoError(t, repo.Create(ctx, &model.FileCode{
		Code: "EXPFAIL01", Text: "x", FilePath: "uploads", UUIDFileName: "x.bin",
		Size: 1, ExpiredCount: -1, ExpiredAt: &past,
	}))

	status, body := performGet(t, "/share/select/", "code=EXPFAIL01",
		ut.Header{Key: "Host", Value: "test.local"})
	require.Equal(t, 404, status, "过期应 404：%s", body)
	require.Contains(t, body, "分享不存在或已过期")

	rows := failureEventRows(t, g, "EXPFAIL01")
	require.Len(t, rows, 1, "过期必须落一条失败事件（可归因）")
	assert.Equal(t, model.PickupFailExpired, rows[0].Reason)
	assert.Equal(t, "EXPFAIL01", rows[0].Code, "必须归因到规范分享码")
}

// TestGetShare_CodeNotFound_NoPickupFailureEvent 负对照：码**确证不存在**
// 不落事件——没有分享行可挂（不可归因），且枚举流量会放大成 DB 写。
// 枚举防护仍走 lockout（core#182 口径，另有测试钉住），本表不掺和。
func TestGetShare_CodeNotFound_NoPickupFailureEvent(t *testing.T) {
	g := newFailureEventEnv(t, true)
	status, body := performGet(t, "/share/select/", "code=NOPEF001",
		ut.Header{Key: "Host", Value: "test.local"})
	require.Equal(t, 404, status, body)
	var n int64
	require.NoError(t, g.Model(&model.PickupFailure{}).Count(&n).Error)
	assert.Zero(t, n, "码不存在不得落失败事件（不可归因）")
}

// TestDownload_CountExhausted_RecordsPickupFailureEvent 下载面（交付点扣次）
// 撞上限：chargePickup 落事件，channel=download-text。
func TestDownload_CountExhausted_RecordsPickupFailureEvent(t *testing.T) {
	g := newFailureEventEnv(t, false) // 令牌关闭 ⇒ 无令牌面直达交付点扣次
	ctx := t.Context()
	repo := dao.NewFileCodeRepository(g)
	require.NoError(t, repo.Create(ctx, &model.FileCode{
		Code: "DLEXH001", Text: "body", ExpiredCount: 0,
	}))

	status, body := performDownloadPath(t, "code=DLEXH001")
	require.Equal(t, 403, status, "交付点扣次撞上限应 403（20004）：%s", body)
	require.Contains(t, body, "超过取件次数")

	rows := failureEventRows(t, g, "DLEXH001")
	require.Len(t, rows, 1, "下载面次数耗尽必须落一条失败事件")
	assert.Equal(t, model.PickupFailCountExhausted, rows[0].Reason)
	assert.Equal(t, "download-text", rows[0].Channel)

	// 计次不变量：这次失败没有扣成，计数保持 0
	fc, err := repo.GetByCode(ctx, "DLEXH001")
	require.NoError(t, err)
	assert.Equal(t, 0, fc.ExpiredCount)
	assert.Equal(t, 0, fc.UsedCount)
}
