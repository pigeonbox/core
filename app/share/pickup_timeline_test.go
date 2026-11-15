package share

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// ==================== 取件失败事件 + 取件时间线（frontend-core#93 复现测试） ====================
//
// 断言原则：每条都要能**变红**——抽掉记录/聚合/属主校验任一环即失败。

// newTimelineEnv 构造带 pickup_failures 表的 share service 测试环境。
func newTimelineEnv(t *testing.T) (*Service, *gorm.DB) {
	t.Helper()
	svc, gormDB, _, _, _ := newTestServiceWithDB(t)
	require.NoError(t, gormDB.AutoMigrate(&model.PickupFailure{}))
	return svc, gormDB
}

// mustShareText 建一条文本分享并返回规范分享码。
func mustShareText(t *testing.T, svc *Service, uid uint, expiredCount int) string {
	t.Helper()
	resp, err := svc.ShareText(context.Background(), &ShareTextReq{
		Text: "timeline", ExpiredCount: expiredCount, UserID: &uid,
	})
	require.NoError(t, err)
	return resp.Code
}

// failureRows 读某分享的全部失败事件（按时间正序）。
func failureRows(t *testing.T, g *gorm.DB, code string) []model.PickupFailure {
	t.Helper()
	var rows []model.PickupFailure
	require.NoError(t, g.Where("code = ?", code).Order("failed_at, id").Find(&rows).Error)
	return rows
}

// TestRecordPickupFailure_NoPlaintextIP_NeverConsumesCount 拍板两条硬边界：
// ① 不落明文 IP（只落粗粒度指纹）；② 失败只记录、不消耗取件次数（计次不变量）。
func TestRecordPickupFailure_NoPlaintextIP_NeverConsumesCount(t *testing.T) {
	svc, gormDB := newTimelineEnv(t)
	ctx := context.Background()
	uid := uint(7)
	code := mustShareText(t, svc, uid, 3)

	svc.RecordPickupFailure(ctx, code, model.PickupFailPasswordWrong, "select", "203.0.113.7")

	rows := failureRows(t, gormDB, code)
	require.Len(t, rows, 1, "失败应落一条事件")
	assert.Equal(t, model.PickupFailPasswordWrong, rows[0].Reason)
	assert.Equal(t, "select", rows[0].Channel)
	assert.False(t, rows[0].FailedAt.IsZero(), "事件时刻必须非零")

	// 隐私：不落明文 IP ⇒ 指纹既不含 IP 也不等于 IP；空 IP ⇒ 空指纹（来源未知）
	assert.NotContains(t, rows[0].SourceHash, "203.0.113.7")
	assert.Len(t, rows[0].SourceHash, 32, "指纹应为 sha256 前 32 hex")
	assert.Regexp(t, "^[0-9a-f]{32}$", rows[0].SourceHash)
	svc.RecordPickupFailure(ctx, code, model.PickupFailExpired, "select", "")
	rows = failureRows(t, gormDB, code)
	require.Len(t, rows, 2)
	assert.Empty(t, rows[1].SourceHash, "空 IP = 来源未知，不造指纹")

	// 计次不变量：两次失败之后次数一分未动
	fc, err := svc.GetFileByCode(ctx, code)
	require.NoError(t, err)
	assert.Equal(t, 3, fc.ExpiredCount, "失败事件不得消耗取件次数（17 条不变量）")
	assert.Equal(t, 0, fc.UsedCount)

	// 同码同 IP ⇒ 指纹稳定（「是否同一来源」的判等基础）；换 IP ⇒ 指纹不同
	svc.RecordPickupFailure(ctx, code, model.PickupFailPasswordWrong, "select", "203.0.113.7")
	svc.RecordPickupFailure(ctx, code, model.PickupFailPasswordWrong, "select", "198.51.100.9")
	rows = failureRows(t, gormDB, code)
	require.Len(t, rows, 4)
	hashes := map[string]int{}
	for _, r := range rows {
		if r.SourceHash != "" {
			hashes[r.SourceHash]++
		}
	}
	assert.Len(t, hashes, 2, "两个来源 = 恰好两个指纹")
	assert.Equal(t, 2, hashes[rows[0].SourceHash], "同 IP 跨两次记录指纹必须一致（首条 203.0.113.7 与其重放）")
}

// TestPickupTimeline_ThreeStates 三态可区分：从未被访问 / 被尝试但失败 / 成功取件。
func TestPickupTimeline_ThreeStates(t *testing.T) {
	svc, gormDB := newTimelineEnv(t)
	ctx := context.Background()
	uid := uint(1)
	code := mustShareText(t, svc, uid, -1)

	// ① 从未被访问
	tl, err := svc.PickupTimeline(ctx, uid, code)
	require.NoError(t, err)
	assert.Equal(t, PickupTimelineStateNever, tl.State)
	assert.Empty(t, tl.Events)
	assert.Zero(t, tl.FailureCount)
	assert.Zero(t, tl.FailureSources)

	// ② 被尝试但失败
	svc.RecordPickupFailure(ctx, code, model.PickupFailCountExhausted, "select", "203.0.113.7")
	tl, err = svc.PickupTimeline(ctx, uid, code)
	require.NoError(t, err)
	assert.Equal(t, PickupTimelineStateFailed, tl.State)
	require.Len(t, tl.Events, 1)
	assert.Equal(t, "failed", tl.Events[0].Result)
	assert.Equal(t, model.PickupFailCountExhausted, tl.Events[0].Reason)
	assert.EqualValues(t, 1, tl.FailureCount)
	assert.EqualValues(t, 1, tl.FailureSources)

	// ③ 成功取件过（失败事件仍在时间线里可见）
	require.NoError(t, gormDB.Create(&model.PickupCharge{
		Code: code, TokenHash: "tok-timeline", Channel: "select",
		IP: "198.51.100.9", ChargedAt: time.Now(),
	}).Error)
	tl, err = svc.PickupTimeline(ctx, uid, code)
	require.NoError(t, err)
	assert.Equal(t, PickupTimelineStatePicked, tl.State)
	require.Len(t, tl.Events, 2, "成功与失败都要在时间线里")
}

// TestPickupTimeline_OrderSameSourceAndWirePrivacy 时间序 + 来源计数/同源判定 +
// wire 隐私（下发面不带任何 IP/指纹）。
func TestPickupTimeline_OrderSameSourceAndWirePrivacy(t *testing.T) {
	svc, gormDB := newTimelineEnv(t)
	ctx := context.Background()
	uid := uint(1)
	code := mustShareText(t, svc, uid, -1)

	base := time.Now().Add(-time.Hour)
	ipA, ipB := failureSourceHash(code, "203.0.113.7"), failureSourceHash(code, "198.51.100.9")
	mk := func(offset time.Duration, reason, hash string) {
		require.NoError(t, gormDB.Create(&model.PickupFailure{
			Code: code, Reason: reason, SourceHash: hash,
			Channel: "select", FailedAt: base.Add(offset),
		}).Error)
	}
	mk(0, model.PickupFailPasswordWrong, ipA)           // 已知来源首条 ⇒ same_source 缺省
	mk(time.Minute, model.PickupFailPasswordWrong, ipA) // 与上一条同源 ⇒ true
	mk(2*time.Minute, model.PickupFailExpired, ipB)     // 换来源 ⇒ false
	require.NoError(t, gormDB.Create(&model.PickupCharge{
		Code: code, TokenHash: "tok-order", Channel: "select",
		IP: "192.0.2.10", ChargedAt: base.Add(3 * time.Minute),
	}).Error)

	tl, err := svc.PickupTimeline(ctx, uid, code)
	require.NoError(t, err)
	require.Len(t, tl.Events, 4, "3 失败 + 1 成功")

	// 时间倒序：成功(最新) → B → A → A
	parsed := make([]time.Time, len(tl.Events))
	for i, ev := range tl.Events {
		tAt, terr := time.Parse(time.RFC3339Nano, ev.At)
		require.NoError(t, terr, "At 必须是 RFC3339Nano")
		parsed[i] = tAt
		if i > 0 {
			assert.True(t, !parsed[i].After(parsed[i-1]), "事件必须按时间倒序")
		}
	}
	assert.Equal(t, "success", tl.Events[0].Result)
	assert.Equal(t, "failed", tl.Events[1].Result)
	assert.Equal(t, model.PickupFailExpired, tl.Events[1].Reason)
	assert.Equal(t, model.PickupFailPasswordWrong, tl.Events[2].Reason)

	// 同源判定：A→A 为 true、A→B 为 false；首条已知来源不下发
	require.NotNil(t, tl.Events[2].SameSource, "第二条失败应给出同源判定")
	assert.True(t, *tl.Events[2].SameSource, "连续两条同 IP 应判同源")
	require.NotNil(t, tl.Events[1].SameSource)
	assert.False(t, *tl.Events[1].SameSource, "换来源应判不同源")

	// 聚合计数：3 次失败、2 个来源
	assert.EqualValues(t, 3, tl.FailureCount)
	assert.EqualValues(t, 2, tl.FailureSources)

	// wire 隐私：序列化后不含任何 IP / 指纹字段
	js, err := json.Marshal(tl)
	require.NoError(t, err)
	s := string(js)
	assert.NotContains(t, s, "source_hash")
	assert.NotContains(t, s, "203.0.113.7")
	assert.NotContains(t, s, "198.51.100.9")
	assert.NotContains(t, s, "192.0.2.10")
}

// TestPickupTimeline_OwnershipAndNotFound 属主校验：不是你的 / 不存在 / 匿名分享
// 一律 ErrTimelineNotFound（统一 404，不泄露存在性）。
func TestPickupTimeline_OwnershipAndNotFound(t *testing.T) {
	svc, _ := newTimelineEnv(t)
	ctx := context.Background()
	owner := uint(1)
	code := mustShareText(t, svc, owner, -1)

	tl, err := svc.PickupTimeline(ctx, owner, code)
	require.NoError(t, err, "属主应能读取")
	assert.Equal(t, PickupTimelineStateNever, tl.State)

	_, err = svc.PickupTimeline(ctx, 999, code)
	assert.ErrorIs(t, err, ErrTimelineNotFound, "非属主必须 404")

	_, err = svc.PickupTimeline(ctx, owner, "NOPE1234")
	assert.ErrorIs(t, err, ErrTimelineNotFound, "不存在的分享必须 404")

	_, err = svc.PickupTimeline(ctx, owner, "")
	assert.ErrorIs(t, err, ErrTimelineNotFound, "空码必须 404")

	// 匿名分享（UserID=nil）：没有属主，谁也读不到
	anonResp, err := svc.ShareText(ctx, &ShareTextReq{Text: "anon", ExpiredCount: -1})
	require.NoError(t, err)
	_, err = svc.PickupTimeline(ctx, 0, anonResp.Code)
	assert.ErrorIs(t, err, ErrTimelineNotFound, "匿名分享无属主 ⇒ 时间线不可达")
}

// TestRecordPickupFailure_UnknownCodeIsSkipped 空码/空原因不落表（防御：
// 不可归因的失败进不了时间线，也不给枚举流量开 DB 写放大面）。
func TestRecordPickupFailure_UnknownCodeIsSkipped(t *testing.T) {
	svc, gormDB := newTimelineEnv(t)
	ctx := context.Background()
	svc.RecordPickupFailure(ctx, "", model.PickupFailExpired, "select", "203.0.113.7")
	svc.RecordPickupFailure(ctx, "WHATEVER1", "", "select", "203.0.113.7")
	var n int64
	require.NoError(t, gormDB.Model(&model.PickupFailure{}).Count(&n).Error)
	assert.Zero(t, n, "空码/空原因不得落表")
}
