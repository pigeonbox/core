package config

// 分享到期提醒（notify.expiry_reminder）的**保存 → 回读 → 全局生效**守卫（5.5.0）。
//
// 链路：PUT /admin/config（UpdateConfigJSON 字段级合并）→ 落库 →
// applySystemConfigOverlayLocked（next.Notify = *sc.Notify 整段赋值）→
// bootstrap 的提醒循环每节拍读 conf.GetGlobalConfig().Notify.ExpiryReminder。
//
// 判据：
//  1. 提交三段字段 ⇒ GetConfig 回读一致，且 conf.Snapshot() 同步（提醒循环读的就是它）；
//  2. 部分提交（只带 enabled）⇒ 同段 advance_hours / interval_minutes **保留**、
//     兄弟渠道（webhook/telegram/bark）也不受牵连——这是「管理台只翻一个开关，
//     另外两个数字被清零」的主回归面（清零 ⇒ 提前量变 0 ⇒ 被 clamp 成默认值，
//     表面看没事，实际把管理员填的窗口悄悄改掉）；
//  3. 未携带 notify 段 ⇒ 整段保留（指针段 nil-overlay），全局值不回弹成零值。
//
// ⚠️ 负对照（改错转红，PR 实测）：把 applySystemConfigOverlayLocked 里
// `next.Notify = *sc.Notify` 删掉 ⇒ ① 的 conf.Snapshot 断言转红（配置写进库但
// 提醒循环永远读到关闭）；把 mergeJSONObjects 的嵌套递归退回「段级整体替换」⇒ ② 转红。
import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/conf"
)

func TestExpiryReminderConfigSaveEffect(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	withGlobalConf(t)

	// ① 提交三个字段 ⇒ 回读一致 + 全局快照同步
	require.NoError(t, svc.UpdateConfigJSON(ctx, []byte(`{"notify":{
		"expiry_reminder":{"enabled":true,"advance_hours":6,"interval_minutes":15}
	}}`)))
	got, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, got.Notify)
	require.True(t, got.Notify.ExpiryReminder.Enabled, "提交→GetConfig 回读应一致")
	require.Equal(t, 6, got.Notify.ExpiryReminder.AdvanceHours)
	require.Equal(t, 15, got.Notify.ExpiryReminder.IntervalMinutes)

	eff := conf.Snapshot().Notify.ExpiryReminder
	require.True(t, eff.Enabled, "保存后提醒循环读到的全局配置必须同步（否则配了但永远不生效）")
	require.Equal(t, 6, eff.AdvanceHours)
	require.Equal(t, 15, eff.IntervalMinutes)

	// ② 部分提交只带 enabled=false ⇒ 两个数字保留，兄弟渠道不受牵连
	require.NoError(t, svc.UpdateConfigJSON(ctx, []byte(`{"notify":{
		"webhook_url":"https://hook.example.com/x",
		"telegram":{"bot_token":"T1","chat_id":"C1"}
	}}`)))
	require.NoError(t, svc.UpdateConfigJSON(ctx, []byte(`{"notify":{"expiry_reminder":{"enabled":false}}}`)))
	got, err = svc.GetConfig(ctx)
	require.NoError(t, err)
	require.False(t, got.Notify.ExpiryReminder.Enabled)
	require.Equal(t, 6, got.Notify.ExpiryReminder.AdvanceHours,
		"只翻开关不得清掉 advance_hours（清零会被 clamp 成默认 24，窗口被悄悄改掉）")
	require.Equal(t, 15, got.Notify.ExpiryReminder.IntervalMinutes, "只翻开关不得清掉 interval_minutes")
	require.Equal(t, "https://hook.example.com/x", got.Notify.WebhookURL, "改提醒不得牵连 webhook")
	require.Equal(t, "T1", got.Notify.Telegram.BotToken, "改提醒不得牵连 telegram")

	eff = conf.Snapshot().Notify.ExpiryReminder
	require.False(t, eff.Enabled, "关掉开关必须立刻反映到全局（不必重启）")
	require.Equal(t, 6, eff.AdvanceHours)
	require.Equal(t, 15, eff.IntervalMinutes)

	// ③ 未携带 notify 段 ⇒ 整段保留，全局值不回弹
	require.NoError(t, svc.UpdateConfigJSON(ctx, []byte(`{"oidc":{}}`)))
	got, err = svc.GetConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, 6, got.Notify.ExpiryReminder.AdvanceHours, "未携带 notify 段应整段保留")
	require.Equal(t, 15, got.Notify.ExpiryReminder.IntervalMinutes)
	require.Equal(t, 6, conf.Snapshot().Notify.ExpiryReminder.AdvanceHours, "全局值不得回弹")
}

// 默认关：bootstrap 的 viper 默认值是 false ⇒ 全局 conf 里该段是零值。
// 这不是「安全默认」的教条，而是「主动打扰」类功能的合规定义：管理员没打算要
// 就不该给全体寄件人发信（与更新检查那种被动查询相反）。
//
// 注意两处真相源不同：提醒循环读的是 conf.Snapshot()（零值即关 ⇒ 判据在
// runExpiryReminderRound 开头直接返回）；GetConfig 在 DB 从未存过 notify 段时
// 返回 nil 段（既有语义，与 p2p 段一致），所以这里只断言全局值。
func TestExpiryReminderDefaultsOff(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	withGlobalConf(t)

	// 存了别的渠道（notify 段已存在）但没碰 expiry_reminder ⇒ 提醒仍是关
	require.NoError(t, svc.UpdateConfigJSON(ctx, []byte(`{"notify":{"webhook_url":"https://hook.example.com/x"}}`)))
	got, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, got.Notify)
	require.False(t, got.Notify.ExpiryReminder.Enabled, "默认不得开启提醒")
	require.False(t, conf.Snapshot().Notify.ExpiryReminder.Enabled,
		"全局配置零值 ⇒ 每轮在判据处直接返回，不会发信")

	// 空全局 + 只存无关段 ⇒ 全局值仍是零值（不会被任何回填意外打开）
	withGlobalConf(t)
	svc2 := newTestConfigService(t)
	require.NoError(t, svc2.UpdateConfigJSON(context.Background(), []byte(`{"oidc":{}}`)))
	require.False(t, conf.Snapshot().Notify.ExpiryReminder.Enabled, "无关段保存不得打开提醒")
}
