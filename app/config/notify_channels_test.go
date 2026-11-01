package config

// Telegram / Bark 外部推送渠道的**保存→生效**端到端守卫（core#31 第二批）。
//
// 链路：PUT /admin/config（UpdateConfigJSON 字段级合并）→ 校验 → 落库 →
// nil-overlay（未携带的段保留）→ 全局 conf overlay → OnNotifyChanged 热重建
// （applyNotifyConfig ⇒ SetTelegram/SetBark）。
//
// 判据（提交 → GetConfig 回读一致 + 热重建被调）：
//  1. 提交 telegram+bark ⇒ 回读一致、热重建收到新值；
//  2. 部分提交（只改 telegram.bot_token）⇒ 同段其余字段（chat_id / bark /
//     smtp）**保留**，热重建收到合并后的全量——这是「保存了但被清掉/不生效」
//     的主回归面；
//  3. 显式 null 清空单渠道 ⇒ 不牵连同段其他渠道；
//  4. 未携带 notify 段 ⇒ 整段保留（指针段 nil-overlay），热重建重放携带
//     不变的保留值（notify/OIDC 钩子为幂等重放语义，与 p2p 的提交意图语义不同）。
//
// ⚠️ 负对照（改错转红，PR 实测）：把 mergeJSONObjects 的嵌套递归退回「段级
// 整体替换」⇒ ② 转红；把 UpdateConfig 的 notify nil-保留删掉 ⇒ ④ 转红。
import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/conf"
)

func TestNotifyChannelsTelegramBarkSaveEffect(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()
	withGlobalConf(t)

	var hookCalls int
	var lastNotify *conf.NotifyConfig
	svc.SetReconfigureHooks(&ReconfigureHooks{
		OnNotifyChanged: func(n *conf.NotifyConfig) { hookCalls++; lastNotify = n },
	})

	// ① 提交 telegram + bark ⇒ 回读一致 + 热重建被调（携带新值）
	require.NoError(t, svc.UpdateConfigJSON(ctx, []byte(`{"notify":{
		"telegram":{"bot_token":"T1","chat_id":"C1"},
		"bark":{"server_url":"https://bark.example.com/push","device_key":"K1"}
	}}`)))
	got, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, got.Notify)
	require.Equal(t, "T1", got.Notify.Telegram.BotToken, "提交→GetConfig 回读应一致")
	require.Equal(t, "C1", got.Notify.Telegram.ChatID)
	require.Equal(t, "https://bark.example.com/push", got.Notify.Bark.ServerURL)
	require.Equal(t, "K1", got.Notify.Bark.DeviceKey)
	require.Equal(t, 1, hookCalls, "保存后热重建应被调一次")
	require.NotNil(t, lastNotify)
	require.Equal(t, "T1", lastNotify.Telegram.BotToken, "热重建应收到新值")
	require.Equal(t, "K1", lastNotify.Bark.DeviceKey)

	// ② 部分提交（只改 telegram.bot_token）⇒ 同段其余字段保留 + 热重建收到合并全量
	require.NoError(t, svc.UpdateConfigJSON(ctx, []byte(`{"notify":{"telegram":{"bot_token":"T2"}}}`)))
	got, err = svc.GetConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, "T2", got.Notify.Telegram.BotToken)
	require.Equal(t, "C1", got.Notify.Telegram.ChatID, "部分提交不得清掉同级 chat_id（字段级合并）")
	require.Equal(t, "K1", got.Notify.Bark.DeviceKey, "部分提交不得清掉同段 bark")
	require.Equal(t, 2, hookCalls)
	require.Equal(t, "T2", lastNotify.Telegram.BotToken)
	require.Equal(t, "C1", lastNotify.Telegram.ChatID, "热重建应收到合并后的全量")

	// ③ 显式 null 清空 bark ⇒ telegram 不受牵连
	require.NoError(t, svc.UpdateConfigJSON(ctx, []byte(`{"notify":{"bark":null}}`)))
	got, err = svc.GetConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, "", got.Notify.Bark.ServerURL, "显式 null 应清空 bark")
	require.Equal(t, "", got.Notify.Bark.DeviceKey)
	require.Equal(t, "T2", got.Notify.Telegram.BotToken, "清 bark 不得牵连 telegram")
	require.Equal(t, 3, hookCalls)
	require.Equal(t, "", lastNotify.Bark.ServerURL, "热重建应收到清空后的 bark（卸载渠道）")

	// ④ 未携带 notify 段 ⇒ 整段保留；热重建照旧幂等重放（现义：notify/OIDC
	// 钩子在任意保存后重放，与 p2p 钩子的「提交意图」语义不同——它只重配
	// 渠道对象，无 stop/start 抖动面），重放必须携带**不变的**保留值。
	require.NoError(t, svc.UpdateConfigJSON(ctx, []byte(`{"oidc":{}}`)))
	got, err = svc.GetConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, "T2", got.Notify.Telegram.BotToken, "未携带 notify 段应整段保留")
	require.Equal(t, "C1", got.Notify.Telegram.ChatID)
	require.GreaterOrEqual(t, hookCalls, 3, "保存后热重建幂等重放（现义）")
	require.Equal(t, "T2", lastNotify.Telegram.BotToken, "重放必须携带保留值，不得清零")
	require.Equal(t, "C1", lastNotify.Telegram.ChatID)
}
