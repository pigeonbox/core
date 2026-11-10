package notify

// 渠道测试发送判据（core#122）：
//
//  ① 假 sender 注入：TestChannel 必须**真实调用**所选渠道的发送实现
//     （不多发、不漏发、不发错渠道），latency 统计随实际耗时走；
//  ② 三渠道真实发送：各渠道固定测试文案真实出网（httptest 收到）+ ok=true；
//  ③ 失败脱敏：失败摘要不得含凭据（bot token / device key / webhook URL），
//     负对照 = 回退 summarizeChannelErr 的两层净化 ⇒ 判据转红（实跑）；
//  ④ 未配置/未知渠道：未配置 ⇒ ok=false + 摘要（不是 API error）；
//     未知渠道 ⇒ ErrInvalidParam（调用方转 400）。
//
// 断言结构事实（键集合/状态/凭据缺席/调用计数），不依赖本地化文案。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============ ① 假 sender 注入：真实调用 + latency 统计 ============

// TestChannelTest_FakeSenderInvokedAndLatencyMeasured 假 sender 注入：
// 只有被选中的渠道发送实现被调用一次，latency_ms 随真实耗时统计。
func TestChannelTest_FakeSenderInvokedAndLatencyMeasured(t *testing.T) {
	for _, channel := range []string{ChannelWebhook, ChannelTelegram, ChannelBark} {
		t.Run(channel, func(t *testing.T) {
			svc := NewService(nil)
			var calls []string
			svc.chanTestSenders = map[string]channelTestSender{}
			for _, name := range []string{ChannelWebhook, ChannelTelegram, ChannelBark} {
				name := name
				svc.chanTestSenders[name] = func(_ *Service, _ context.Context) error {
					calls = append(calls, name)
					if name == channel {
						time.Sleep(25 * time.Millisecond) // 让 latency 可观测
					}
					return nil
				}
			}

			result, err := svc.TestChannel(context.Background(), channel)
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.True(t, result.OK)
			assert.Equal(t, []string{channel}, calls, "只调用所选渠道的发送实现一次")
			assert.GreaterOrEqual(t, result.LatencyMS, int64(20),
				"latency_ms 必须反映真实耗时（假 sender 睡了 25ms）")
			assert.Empty(t, result.ErrorSummary, "成功时摘要为空")
		})
	}
}

// TestChannelTest_FailureAlsoMeasured 发送失败同样统计 latency 且返回脱敏摘要。
func TestChannelTest_FailureAlsoMeasured(t *testing.T) {
	svc := NewService(nil)
	svc.chanTestSenders = map[string]channelTestSender{
		ChannelWebhook: func(_ *Service, _ context.Context) error {
			time.Sleep(15 * time.Millisecond)
			return errors.New("boom")
		},
	}
	result, err := svc.TestChannel(context.Background(), ChannelWebhook)
	require.NoError(t, err, "渠道失败不是 API error")
	require.NotNil(t, result)
	assert.False(t, result.OK)
	assert.GreaterOrEqual(t, result.LatencyMS, int64(10))
	assert.NotEmpty(t, result.ErrorSummary)
}

// ============ ② 三渠道真实发送（固定测试文案真实出网） ============

// TestChannelTest_RealSendAllThreeChannels webhook/telegram/bark 各真实发一条，
// httptest 收到固定测试文案；成功 ⇒ ok=true + latency_ms 有值。
func TestChannelTest_RealSendAllThreeChannels(t *testing.T) {
	hook, hookRec := newRecordingServer(t, http.StatusOK)
	tg, tgRec := newRecordingServer(t, http.StatusOK)
	bark, barkRec := newRecordingServer(t, http.StatusOK)
	allowPrivatePush(t) // httptest 是 127.0.0.1

	svc := NewService(nil)
	svc.SetWebhookURL(hook.URL)
	svc.SetTelegram(&TelegramConfig{BotToken: "tok", ChatID: "42", apiBase: tg.URL})
	svc.SetBark(&BarkConfig{ServerURL: bark.URL, DeviceKey: "devkey"})

	// webhook：结构化 payload（event=notify.test + 固定文案）
	result, err := svc.TestChannel(context.Background(), ChannelWebhook)
	require.NoError(t, err)
	require.True(t, result.OK)
	got := waitPush(t, hookRec)
	assert.Equal(t, "notify.test", got.Body["event"])
	assert.Equal(t, channelTestTitle, got.Body["title"])
	assert.Equal(t, channelTestContent, got.Body["content"])

	// telegram：POST /bot<token>/sendMessage，text 含固定文案
	result, err = svc.TestChannel(context.Background(), ChannelTelegram)
	require.NoError(t, err)
	require.True(t, result.OK)
	got = waitPush(t, tgRec)
	assert.Contains(t, got.Path, "/bottok/sendMessage")
	assert.Contains(t, got.Body["text"], channelTestTitle)

	// bark：POST /<device_key>，title/body 为固定文案
	result, err = svc.TestChannel(context.Background(), ChannelBark)
	require.NoError(t, err)
	require.True(t, result.OK)
	got = waitPush(t, barkRec)
	assert.Equal(t, "/devkey", got.Path)
	assert.Equal(t, channelTestTitle, got.Body["title"])
	assert.Equal(t, channelTestContent, got.Body["body"])
}

// ============ ③ 失败摘要脱敏（正判据 + 负对照） ============

// TestChannelTest_ErrorSummaryMasksCredentials 失败摘要不得含凭据。
// 三条失败形态各一发（验收：失败用错 token/凭据，断言错误摘要不含凭据）：
//   - telegram：服务端 401 且回显 token（最坏形态：错误体带凭据）；
//   - bark：服务端 404 且回显 device key；
//   - webhook：目标不可达（传输层 *url.Error，原文带完整目标 URL）。
func TestChannelTest_ErrorSummaryMasksCredentials(t *testing.T) {
	allowPrivatePush(t)

	t.Run("telegram", func(t *testing.T) {
		const token = "123456:SECRET-TG-TOKEN"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprintf(w, "unauthorized for %s", token) // 最坏形态：错误体回显凭据
		}))
		t.Cleanup(srv.Close)

		svc := NewService(nil)
		svc.SetTelegram(&TelegramConfig{BotToken: token, ChatID: "42", apiBase: srv.URL})
		result, err := svc.TestChannel(context.Background(), ChannelTelegram)
		require.NoError(t, err)
		require.False(t, result.OK)
		assert.NotEmpty(t, result.ErrorSummary)
		assert.NotContains(t, result.ErrorSummary, token, "摘要不得含 bot token")
	})

	t.Run("bark", func(t *testing.T) {
		const key = "SECRET-DEVICE-KEY"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, "device %s not found", key) // 最坏形态：错误体回显凭据
		}))
		t.Cleanup(srv.Close)

		svc := NewService(nil)
		svc.SetBark(&BarkConfig{ServerURL: srv.URL, DeviceKey: key})
		result, err := svc.TestChannel(context.Background(), ChannelBark)
		require.NoError(t, err)
		require.False(t, result.OK)
		assert.NotEmpty(t, result.ErrorSummary)
		assert.NotContains(t, result.ErrorSummary, key, "摘要不得含 device key")
	})

	t.Run("webhook", func(t *testing.T) {
		// 起一个即关的服务器 ⇒ 目标不可达；URL 路径内嵌凭据（Slack/Discord 形态）
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		target := srv.URL + "/services/SECRET-HOOK-TOKEN"
		srv.Close()

		svc := NewService(nil)
		svc.SetWebhookURL(target)
		result, err := svc.TestChannel(context.Background(), ChannelWebhook)
		require.NoError(t, err)
		require.False(t, result.OK)
		assert.NotEmpty(t, result.ErrorSummary)
		assert.NotContains(t, result.ErrorSummary, "SECRET-HOOK-TOKEN", "摘要不得含 webhook 凭据段")
		assert.NotContains(t, result.ErrorSummary, target, "摘要不得回显完整目标 URL")
	})
}

// TestChannelTest_SummarySanitization 脱敏层的直接判据（负对照锚点）：
// 假 sender 返回**内嵌凭据**的错误 ⇒ 摘要必须打码。
//
// 负对照（实跑）：把 summarizeChannelErr 的凭据打码段回退 ⇒ 本用例转红。
func TestChannelTest_SummarySanitization(t *testing.T) {
	const token = "FAKE-BOT-TOKEN-999"
	svc := NewService(nil)
	svc.SetTelegram(&TelegramConfig{BotToken: token, ChatID: "42"})
	svc.chanTestSenders = map[string]channelTestSender{
		ChannelTelegram: func(_ *Service, _ context.Context) error {
			return fmt.Errorf("upstream said: bad token %s rejected", token)
		},
	}
	result, err := svc.TestChannel(context.Background(), ChannelTelegram)
	require.NoError(t, err)
	require.False(t, result.OK)
	assert.NotContains(t, result.ErrorSummary, token, "凭据串必须被打码")
	assert.NotEmpty(t, result.ErrorSummary, "脱敏不等于清空：摘要仍需可排障")
}

// TestChannelTest_UrlErrorStripsTargetURL 第一层净化的直接判据：
// *url.Error 形态的错误（原文带完整目标 URL）⇒ 摘要只留 Op + 内层原因。
//
// 判据刻意用 **URL 转义形态**的凭据（device key 空格/斜杠 ⇒ 路径里是
// dev%20key%2F1）：第二层打码只认配置原串、盖不住转义形态 ⇒ 只有第一层
// （剥 URL）能拦住 ⇒ 负对照可判别。
//
// 负对照（实跑）：回退 url.Error 剥离 ⇒ 摘要带转义凭据/URL ⇒ 本用例转红。
func TestChannelTest_UrlErrorStripsTargetURL(t *testing.T) {
	const rawKey = "dev key/1"
	svc := NewService(nil)
	svc.SetBark(&BarkConfig{ServerURL: "https://bark.example.invalid", DeviceKey: rawKey})
	svc.chanTestSenders = map[string]channelTestSender{
		ChannelBark: func(_ *Service, _ context.Context) error {
			// 真实 sendBark 的传输层错误形态（url.Error 原文带完整 URL，
			// device key 在路径里是转义形态）
			return &url.Error{Op: "Post", URL: barkPushURL(svc.bark()), Err: errors.New("connection refused")}
		},
	}
	result, err := svc.TestChannel(context.Background(), ChannelBark)
	require.NoError(t, err)
	require.False(t, result.OK)
	assert.NotContains(t, result.ErrorSummary, "dev%20key%2F1", "转义形态的凭据不得回显")
	assert.NotContains(t, result.ErrorSummary, "https://")
	assert.NotContains(t, result.ErrorSummary, "bark.example.invalid")
	assert.NotEmpty(t, result.ErrorSummary)
}

// ============ ④ 未配置 / 未知渠道 ============

// TestChannelTest_NotConfigured 未配置渠道 ⇒ ok=false + 摘要（不是 API error）。
func TestChannelTest_NotConfigured(t *testing.T) {
	svc := NewService(nil) // 三渠道均未注入配置
	for _, channel := range []string{ChannelWebhook, ChannelTelegram, ChannelBark} {
		result, err := svc.TestChannel(context.Background(), channel)
		require.NoError(t, err, "未配置是测试结论，不是 API error")
		require.NotNil(t, result)
		assert.False(t, result.OK)
		assert.NotEmpty(t, result.ErrorSummary)
	}
}

// TestChannelTest_UnknownChannel 未知渠道 ⇒ ErrInvalidParam（调用方转 400）。
func TestChannelTest_UnknownChannel(t *testing.T) {
	svc := NewService(nil)
	result, err := svc.TestChannel(context.Background(), "sms")
	assert.Nil(t, result)
	assert.True(t, errors.Is(err, ErrInvalidParam))
}
