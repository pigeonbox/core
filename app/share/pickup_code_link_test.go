package share

import (
	"context"
	"testing"
	"time"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 取件码也能当分享页链接用（2026-10-23 新能力）。
//
// 此前取件码（6 位）只能走匿名通道 `/anonymous/retrieve` 取件，**没有页面链接**
// ——分享页路由 `/share/:code` → `/share/select/?code=` 只认 8 位分享码
// （`file_codes.code`），拿取件码去查必然 404。
//
// 本轮把分享域的「按码取分享」（GetFileByCode）扩成**同时认分享码与取件码**
// （取件码真相源 = `file_codes.pickup_code` 列，uniqueIndex），落地后
// `/#/share/{取件码}` 可直接打开取件页。
//
// 配套的两条边界（跨列避让）也在这里钉住：自定义分享码与取件码撞串会让
// 「按码取分享」打开别人的分享 ⇒ 铸造取件码与创建自定义码两侧都要避让。

func TestGetFileByCode_PickupCodeFallback(t *testing.T) {
	svc, gormDB, _, _, _ := newTestServiceWithDB(t)
	ctx := context.Background()

	require.NoError(t, gormDB.Create(&model.FileCode{
		Code: "AbCd1234", Status: model.StatusNormal, ExpiredCount: -1,
	}).Error)
	// 给同一条记录铸一个 6 位取件码
	require.NoError(t, gormDB.Model(&model.FileCode{}).
		Where("code = ?", "AbCd1234").
		UpdateColumn("pickup_code", "WS9WHP").Error)

	t.Run("取件码可取到分享（落地取件页链接）", func(t *testing.T) {
		fc, err := svc.GetFileByCode(ctx, "WS9WHP")
		require.NoError(t, err)
		assert.Equal(t, "AbCd1234", fc.Code) // 归一为 8 位分享码
	})

	t.Run("取件码大小写不敏感（与匿名域 lookupShareCode 同口径）", func(t *testing.T) {
		fc, err := svc.GetFileByCode(ctx, "ws9whp")
		require.NoError(t, err)
		assert.Equal(t, "AbCd1234", fc.Code)
	})

	t.Run("取件码两侧空白容忍", func(t *testing.T) {
		fc, err := svc.GetFileByCode(ctx, "  WS9WHP  ")
		require.NoError(t, err)
		assert.Equal(t, "AbCd1234", fc.Code)
	})

	t.Run("未知码仍报不存在（不回落成假命中）", func(t *testing.T) {
		_, err := svc.GetFileByCode(ctx, "ZZZZZZ")
		assert.Error(t, err)
	})

	t.Run("8 位输入不走去件码列（省一次索引查询，语义更清晰）", func(t *testing.T) {
		_, err := svc.GetFileByCode(ctx, "AbCd1234")
		require.NoError(t, err)
	})
}

func TestGetFileByCode_PickupCodeInheritsGuards(t *testing.T) {
	svc, gormDB, _, _, _ := newTestServiceWithDB(t)
	ctx := context.Background()

	// 过期分享
	require.NoError(t, gormDB.Create(&model.FileCode{
		Code: "Exp1234", Status: model.StatusNormal, ExpiredCount: -1,
		ExpiredAt: func() *time.Time { p := time.Now().Add(-time.Hour); return &p }(),
	}).Error)
	require.NoError(t, gormDB.Model(&model.FileCode{}).
		Where("code = ?", "Exp1234").UpdateColumn("pickup_code", "EXP001").Error)

	// 被管控的分享
	require.NoError(t, gormDB.Create(&model.FileCode{
		Code: "Blk1234", Status: model.StatusBlocked, ExpiredCount: -1,
	}).Error)
	require.NoError(t, gormDB.Model(&model.FileCode{}).
		Where("code = ?", "Blk1234").UpdateColumn("pickup_code", "BLK001").Error)

	t.Run("过期取件码同样拒取（判定与分享码共用）", func(t *testing.T) {
		_, err := svc.GetFileByCode(ctx, "EXP001")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "expired")
	})

	t.Run("被管控取件码同样拒取（typed error）", func(t *testing.T) {
		_, err := svc.GetFileByCode(ctx, "BLK001")
		require.Error(t, err)
		var blocked *ShareBlockedError
		assert.ErrorAs(t, err, &blocked)
	})
}

func TestCreateWithCode_RejectsPickupCodeCollision(t *testing.T) {
	svc, gormDB, _, _, _ := newTestServiceWithDB(t)
	ctx := context.Background()

	// 某分享已铸取件码 WS9WHP
	require.NoError(t, gormDB.Create(&model.FileCode{
		Code: "AbCd1234", Status: model.StatusNormal, ExpiredCount: -1,
	}).Error)
	require.NoError(t, gormDB.Model(&model.FileCode{}).
		Where("code = ?", "AbCd1234").UpdateColumn("pickup_code", "WS9WHP").Error)

	build := func(code string) *model.FileCode {
		return &model.FileCode{Code: code, Status: model.StatusNormal}
	}

	// 跨列避让：自定义码不能占用取件码已在用的字符串（否则那个取件码会打开本分享）
	_, err := svc.createWithCode(ctx, "WS9WHP", build)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "取件码")

	// 大小写变体同样算撞串（取件解析时输入会被 ToUpper）
	_, err = svc.createWithCode(ctx, "ws9whp", build)
	require.Error(t, err)

	// 不冲突的自定义码照常创建
	fc, err := svc.createWithCode(ctx, "MyOwnCode", build)
	require.NoError(t, err)
	assert.Equal(t, "MyOwnCode", fc.Code)
}
