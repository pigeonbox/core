package notify

// Telegram/Bark 外部推送渠道守卫（2026-11-01，core#31）。
//
// 设计稿：docs/specs/2026-11-01-notify-channels-telegram-bark-design.md。
// 三组判据，均带负对照（改对转绿 / 改错转红，PR 里附实测记录）：
//
//  ① SSRF：Bark ServerURL 由用户填 ⇒ 发送前复跑 security.ValidateEndpointURL，
//     私网/环回默认拒（**不开 allow_private 后门**）；负对照 = 合法公网过 +
//     全局 security.ssrf.allow_private_networks 开启后放行（同一口径）；
//     Telegram host 固定 api.telegram.org（拼好的 URL 仍过校验 = 纵深防御）。
//  ② fan-out：webhook/telegram/bark 各自 best-effort，一渠道失败（HTTP 500）
//     不影响他渠道；全部禁用时短路不发。
//  ③ data race：telegram/bark 配置运行期热重建 + 并发读 ⇒ -race 不红、
//     读到的配置不撕裂（同 notify_concurrency_test.go 的口径）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/security"
)

// ============ ① SSRF ============

// TestExternal_SSRF_BarkRejectsPrivateTargets Bark 私网目标发送前必被拒。
//
// 判据：sendBark 返回的错误必须**包裹 security.ErrEndpointURL**（= 校验层拒）。
// 负对照：若去掉发送前校验，错误会变成连接层的 *url.Error（connection refused）
// ⇒ 本用例转红。
func TestExternal_SSRF_BarkRejectsPrivateTargets(t *testing.T) {
	svc := NewService(nil)
	for _, raw := range []string{
		"http://127.0.0.1:9000",
		"http://10.0.0.5:8080",
		"http://192.168.1.10:9000",
		"http://169.254.169.254/latest/meta-data", // 云元数据端点（SSRF 经典目标）
	} {
		cfg := &BarkConfig{ServerURL: raw, DeviceKey: "devkey"}
		err := svc.sendBark(context.Background(), cfg, "t", "b")
		require.Error(t, err, "私网/保留地址目标 %s 必须被拒", raw)
		assert.True(t, errors.Is(err, security.ErrEndpointURL),
			"%s 应被 SSRF 校验层拒绝（ErrEndpointURL），实际: %v", raw, err)
	}
}

// TestExternal_SSRF_BarkPublicTargetPasses 负对照：合法公网目标过校验层
// （IP 字面量判定，无 DNS/网络依赖）。
func TestExternal_SSRF_BarkPublicTargetPasses(t *testing.T) {
	target := barkPushURL(&BarkConfig{ServerURL: "https://1.1.1.1", DeviceKey: "devkey"})
	assert.Equal(t, "https://1.1.1.1/devkey", target)
	assert.NoError(t, security.ValidateEndpointURL(target), "公网目标必须过校验")
}

// TestExternal_SSRF_BarkPrivateAllowedOnlyByGlobalSwitch 负对照：全局
// security.ssrf.allow_private_networks 开启后私网放行（同一全局口径，
// 不是 Bark 专属后门）——此时请求真正发出（httptest 收到）。
func TestExternal_SSRF_BarkPrivateAllowedOnlyByGlobalSwitch(t *testing.T) {
	srv, rec := newRecordingServer(t, http.StatusOK)
	allowPrivatePush(t)

	svc := NewService(nil)
	svc.SetBark(&BarkConfig{ServerURL: srv.URL, DeviceKey: "devkey"})
	require.NoError(t, svc.sendBark(context.Background(), svc.bark(), "标题", "内容"))

	got := waitPush(t, rec)
	assert.Equal(t, "/devkey", got.Path)
	assert.Equal(t, "标题", got.Body["title"])
	assert.Equal(t, "内容", got.Body["body"])
}

// TestExternal_SSRF_TelegramHostPinned Telegram 目标 host 固定 api.telegram.org
// （不可由用户输入），且拼好的 URL 仍过 ValidateEndpointURL（纵深防御）。
func TestExternal_SSRF_TelegramHostPinned(t *testing.T) {
	target := telegramPushURL(&TelegramConfig{BotToken: "TOKEN", ChatID: "42"})
	assert.Equal(t, "https://api.telegram.org/botTOKEN/sendMessage", target)

	u, err := url.Parse(target)
	require.NoError(t, err)
	assert.Equal(t, "api.telegram.org", u.Host, "host 必须钉死 api.telegram.org")
	assert.NoError(t, security.ValidateEndpointURL(target), "拼好的 URL 仍须过校验（纵深防御）")
}

// ============ ② fan-out ============

// TestExternal_FanOut_AllChannelsReceive 三渠道各自收到（fire-and-forget）。
func TestExternal_FanOut_AllChannelsReceive(t *testing.T) {
	hook, hookRec := newRecordingServer(t, http.StatusOK)
	tg, tgRec := newRecordingServer(t, http.StatusOK)
	bark, barkRec := newRecordingServer(t, http.StatusOK)
	allowPrivatePush(t)

	svc := NewService(nil)
	svc.SetWebhookURL(hook.URL)
	svc.SetTelegram(&TelegramConfig{BotToken: "tok", ChatID: "42", apiBase: tg.URL})
	svc.SetBark(&BarkConfig{ServerURL: bark.URL, DeviceKey: "devkey"})

	payload := map[string]interface{}{"event": "notify.created", "title": "标题"}
	svc.dispatchExternal(context.Background(), "notify.created", payload, "标题", "内容")

	got := waitPush(t, hookRec)
	assert.Equal(t, "notify.created", got.Body["event"])
	assert.Equal(t, "标题", got.Body["title"])

	gotTg := waitPush(t, tgRec)
	assert.Equal(t, "/bottok/sendMessage", gotTg.Path)
	assert.Equal(t, "42", gotTg.Body["chat_id"])
	assert.Equal(t, "标题\n\n内容", gotTg.Body["text"])

	gotBark := waitPush(t, barkRec)
	assert.Equal(t, "/devkey", gotBark.Path)
	assert.Equal(t, "标题", gotBark.Body["title"])
	assert.Equal(t, "内容", gotBark.Body["body"])
}

// TestExternal_FanOut_ChannelFailureIsolated 一渠道失败（HTTP 500）不影响他渠道。
//
// 负对照：若把 fan-out 改成"顺序发送、遇错提前返回"⇒ 挂掉渠道排前面时
// 后面渠道收不到 ⇒ 本用例转红。
func TestExternal_FanOut_ChannelFailureIsolated(t *testing.T) {
	cases := []string{"webhook", "telegram", "bark"}
	for _, failing := range cases {
		t.Run(failing+" fails", func(t *testing.T) {
			status := func(name string) int {
				if name == failing {
					return http.StatusInternalServerError
				}
				return http.StatusOK
			}
			hook, hookRec := newRecordingServer(t, status("webhook"))
			tg, tgRec := newRecordingServer(t, status("telegram"))
			bark, barkRec := newRecordingServer(t, status("bark"))
			allowPrivatePush(t)

			svc := NewService(nil)
			svc.SetWebhookURL(hook.URL)
			svc.SetTelegram(&TelegramConfig{BotToken: "tok", ChatID: "42", apiBase: tg.URL})
			svc.SetBark(&BarkConfig{ServerURL: bark.URL, DeviceKey: "devkey"})

			payload := map[string]interface{}{"event": "notify.created"}
			svc.dispatchExternal(context.Background(), "notify.created", payload, "标题", "内容")

			// 健康渠道必须都收到（挂掉的渠道 best-effort 失败只记日志）
			if failing != "webhook" {
				_ = waitPush(t, hookRec)
			}
			if failing != "telegram" {
				_ = waitPush(t, tgRec)
			}
			if failing != "bark" {
				_ = waitPush(t, barkRec)
			}
		})
	}
}

// TestExternal_FanOut_AllDisabledShortCircuits 全部渠道未配置 ⇒ 短路，不发任何请求。
func TestExternal_FanOut_AllDisabledShortCircuits(t *testing.T) {
	hook, hookRec := newRecordingServer(t, http.StatusOK)
	svc := NewService(nil)
	svc.SetWebhookURL(hook.URL)
	svc.SetTelegram(nil)
	svc.SetBark(nil)
	svc.SetWebhookURL("") // 全部禁用

	svc.dispatchExternal(context.Background(), "notify.created", map[string]interface{}{}, "t", "c")

	select {
	case rec := <-hookRec:
		t.Fatalf("全渠道禁用时不得发出任何请求，却收到 %+v", rec)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestExternal_SettersNormalizeDisabled SetTelegram/SetBark 缺任一字段 = 禁用。
func TestExternal_SettersNormalizeDisabled(t *testing.T) {
	svc := NewService(nil)

	svc.SetTelegram(&TelegramConfig{BotToken: " tok ", ChatID: " 42 "})
	require.NotNil(t, svc.telegram())
	assert.Equal(t, "tok", svc.telegram().BotToken, "应去首尾空白")
	assert.Equal(t, "42", svc.telegram().ChatID)

	svc.SetTelegram(&TelegramConfig{BotToken: "tok"}) // 缺 chat_id
	assert.Nil(t, svc.telegram(), "缺任一字段 = 禁用")
	svc.SetTelegram(nil)
	assert.Nil(t, svc.telegram())

	svc.SetBark(&BarkConfig{ServerURL: "https://push.example.com/", DeviceKey: " key "})
	require.NotNil(t, svc.bark())
	assert.Equal(t, "key", svc.bark().DeviceKey)

	svc.SetBark(&BarkConfig{ServerURL: "https://push.example.com/"}) // 缺 device_key
	assert.Nil(t, svc.bark(), "缺任一字段 = 禁用")
	svc.SetBark(nil)
	assert.Nil(t, svc.bark())
}

// ============ ③ data race（配合 -race） ============

// TestExternal_ConcurrentHotRebuildNoRace telegram/bark 配置运行期热重建 +
// 并发读 ⇒ -race 不红，且读到的配置不撕裂（要么 nil、要么完整一条）。
//
// 负对照：把 atomic.Pointer 退回裸字段写法 ⇒ -race 必报 DATA RACE（实测记录见 PR）。
func TestExternal_ConcurrentHotRebuildNoRace(t *testing.T) {
	svc := NewService(nil)

	const writers = 4
	const readers = 8
	const rounds = 300

	var wg sync.WaitGroup
	start := make(chan struct{})
	broken := make(chan string, 32)

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := 0; i < rounds; i++ {
				if i%2 == 0 {
					svc.SetTelegram(&TelegramConfig{
						BotToken: fmt.Sprintf("tok-%d-%d", w, i),
						ChatID:   fmt.Sprintf("chat-%d-%d", w, i),
					})
					svc.SetBark(&BarkConfig{
						ServerURL: fmt.Sprintf("https://bark-%d-%d.example.com", w, i),
						DeviceKey: fmt.Sprintf("key-%d-%d", w, i),
					})
				} else {
					svc.SetTelegram(nil)
					svc.SetBark(nil)
				}
			}
		}(w)
	}

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < rounds; i++ {
				if tg := svc.telegram(); tg != nil {
					if !strings.HasPrefix(tg.BotToken, "tok-") || !strings.HasPrefix(tg.ChatID, "chat-") {
						select {
						case broken <- "telegram: " + tg.BotToken + "/" + tg.ChatID:
						default:
						}
					}
				}
				if br := svc.bark(); br != nil {
					if !strings.HasPrefix(br.ServerURL, "https://bark-") || !strings.HasPrefix(br.DeviceKey, "key-") {
						select {
						case broken <- "bark: " + br.ServerURL + "/" + br.DeviceKey:
						default:
						}
					}
				}
			}
		}()
	}

	close(start)
	wg.Wait()
	close(broken)

	for got := range broken {
		t.Errorf("读到的渠道配置必须是完整的一条（撕裂读），却得到 %q", got)
	}
}

// ============ 测试脚手架 ============

// pushRecord 收到的一次推送。
type pushRecord struct {
	Path string
	Body map[string]string
}

// newRecordingServer 起一个记录请求的 httptest 服务（响应指定状态码）。
func newRecordingServer(t *testing.T, status int) (*httptest.Server, <-chan pushRecord) {
	t.Helper()
	ch := make(chan pushRecord, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]string
		_ = json.Unmarshal(raw, &m)
		select {
		case ch <- pushRecord{Path: r.URL.Path, Body: m}:
		default:
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, ch
}

// waitPush 等一次推送（fire-and-forget ⇒ 轮询等异步 goroutine）。
func waitPush(t *testing.T, ch <-chan pushRecord) pushRecord {
	t.Helper()
	select {
	case rec := <-ch:
		return rec
	case <-time.After(5 * time.Second):
		t.Fatal("5s 内未收到预期推送（fire-and-forget goroutine 未送达）")
		return pushRecord{}
	}
}

// allowPrivatePush 全局开 security.ssrf.allow_private_networks（测试内 httptest
// 是 127.0.0.1）；用完恢复。
func allowPrivatePush(t *testing.T) {
	t.Helper()
	prev := conf.GetGlobalConfig()
	conf.SetGlobalConfig(&conf.AppConfiguration{
		Security: conf.SecurityConfig{
			SSRF: conf.SSRFConfig{AllowPrivateNetworks: true},
		},
	})
	t.Cleanup(func() { conf.SetGlobalConfig(prev) })
}

// ============ ④ 凭据不落日志 ============

// TestExternal_CredentialNotLeakedInPushError 推送失败时，错误文案**不得**含凭据。
//
// 判据（core#31 验收项「凭据不出现在日志里」）：推送目标 URL **内嵌凭据**
// ——Telegram 的 bot token 在 /bot<token>/ 路径里，Bark 的 device key 在路径
// 末段 ⇒ 传输层失败是 *url.Error，其 Error() 带**完整 URL**
// ⇒ 原样落进 dispatchExternal 的 "err" 字段 = **明文凭据泄漏进日志**。
//
// 负对照：去掉错误净化 ⇒ 错误串含 token / device_key ⇒ 本用例转红。
func TestExternal_CredentialNotLeakedInPushError(t *testing.T) {
	// 放行私网 ⇒ 失败点落在**传输层**（而不是被 SSRF 层先拒），才能造出
	// 真正带 URL 的 *url.Error。
	allowPrivatePush(t)

	// 起一个 server 再关掉 ⇒ 端口无人监听 ⇒ connection refused（*url.Error）
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	const botToken = "test-bot-token"
	const deviceKey = "test-device-key"

	svc := NewService(nil)

	tgErr := svc.sendTelegram(context.Background(),
		&TelegramConfig{BotToken: botToken, ChatID: "42", apiBase: deadURL}, "标题", "内容")
	require.Error(t, tgErr, "目标不可达 ⇒ 必须报错（有可读错误面，不是静默吞掉）")
	assert.Contains(t, tgErr.Error(), "telegram", "错误文案要能看出是哪个渠道")
	assert.NotContains(t, tgErr.Error(), botToken, "telegram 错误文案不得含 bot token")
	assert.NotContains(t, tgErr.Error(), deadURL, "telegram 错误文案不得含完整目标 URL")

	barkErr := svc.sendBark(context.Background(),
		&BarkConfig{ServerURL: deadURL, DeviceKey: deviceKey}, "标题", "内容")
	require.Error(t, barkErr, "目标不可达 ⇒ 必须报错")
	assert.Contains(t, barkErr.Error(), "bark", "错误文案要能看出是哪个渠道")
	assert.NotContains(t, barkErr.Error(), deviceKey, "bark 错误文案不得含 device key")
	assert.NotContains(t, barkErr.Error(), deadURL, "bark 错误文案不得含完整目标 URL")
}

// TestExternal_CredentialNotLeakedOnStatusError 非 2xx 的失败面同样不得含凭据
// （负对照：StatusError 的 Body 是服务端响应体，理论上不含凭据；但若有人
// 把目标 URL 拼进错误体，也必须被打码）。
func TestExternal_CredentialNotLeakedOnStatusError(t *testing.T) {
	allowPrivatePush(t)

	srv, _ := newRecordingServer(t, http.StatusInternalServerError)
	svc := NewService(nil)

	err := svc.sendTelegram(context.Background(),
		&TelegramConfig{BotToken: "test-bot-token", ChatID: "42", apiBase: srv.URL},
		"标题", "内容")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "test-bot-token",
		"HTTP 失败面也不得含凭据")
}
