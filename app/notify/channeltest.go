// 渠道测试发送（core#122）：webhook / telegram / bark 三个外部推送渠道的
// 「配完先试一条」反馈回路。
//
// 为什么需要：三渠道配置项多（webhook URL、bot_token、chat_id、device_key、
// server_url），配错一个字符就是静默失败——此前发送失败只出现在真实事件触发时
// （到件通知/到期提醒），反馈回路太长；SMTP / OIDC 已有测试端点，推送三渠道没有。
//
// 语义：
//   - 用固定测试文案向该渠道**真实发一条**（复用各渠道既有发送链
//     sendWebhook / sendTelegram / sendBark，含 SSRF 复验与凭据净化）；
//   - 同步等待结果并测耗时；渠道发送失败是「测试的结论」（ok=false +
//     error_summary），不是 API 错误（HTTP 仍 200）；
//   - error_summary **必须脱敏**：Telegram bot token / Bark device key /
//     webhook 目标 URL（常内嵌凭据路径）绝不回给管理台。
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"
)

// 渠道名（wire 取值 = POST /admin/notify/channels/test 的 channel 字段）。
const (
	ChannelWebhook  = "webhook"
	ChannelTelegram = "telegram"
	ChannelBark     = "bark"
)

// 固定测试文案（core#122 期望：固定测试文案真实发一条）。
const (
	channelTestTitle   = "PigeonBox 通知渠道测试"
	channelTestContent = "这是一条来自 PigeonBox 的通知渠道测试消息，收到即代表该渠道配置生效。"
)

// ErrChannelNotConfigured 渠道未配置或配置不完整（缺任一必填项即视为未启用，
// 与 SetWebhookURL / SetTelegram / SetBark 的启用判定同口径）。
var ErrChannelNotConfigured = errors.New("channel not configured")

// ChannelTestResult 渠道测试发送结果
// （POST /admin/notify/channels/test 成功响应 data 的 wire 形状）。
type ChannelTestResult struct {
	// OK 该渠道是否真实送达（发送链回 nil）。
	OK bool `json:"ok"`
	// LatencyMS 从发起发送到收到结果的耗时毫秒数（含网络往返与 SSRF 校验）。
	LatencyMS int64 `json:"latency_ms"`
	// ErrorSummary 失败摘要（ok=false 时非空）。**已脱敏**：不含 token / key /
	// 目标 URL 等凭据片段（见 summarizeChannelErr）。
	ErrorSummary string `json:"error_summary"`
}

// channelTestSender 单渠道测试发送实现（复用既有真实发送链）。
type channelTestSender func(s *Service, ctx context.Context) error

// chanTestSenders 渠道名 → 测试发送实现的分发表（NewService 装配真实现；
// 测试整表替换做**假 sender 注入**，验证真实调用与 latency 统计）。
// 按实例持有而非包级变量：热重建/多实例下互不干扰。
func (s *Service) initChannelTestSenders() {
	s.chanTestSenders = map[string]channelTestSender{
		ChannelWebhook:  (*Service).sendTestWebhook,
		ChannelTelegram: (*Service).sendTestTelegram,
		ChannelBark:     (*Service).sendTestBark,
	}
}

// TestChannel 向指定渠道真实发送一条固定测试消息并测量耗时。
//
// 返回值约定：未知渠道 ⇒ (nil, ErrInvalidParam)（调用方转 400）；
// 其余情况**恒不返 error**——渠道发送失败/未配置是测试的结论，落在
// ChannelTestResult（ok=false + 脱敏摘要）。
func (s *Service) TestChannel(ctx context.Context, channel string) (*ChannelTestResult, error) {
	send, ok := s.chanTestSenders[channel]
	if !ok {
		return nil, ErrInvalidParam
	}
	start := time.Now()
	err := send(s, ctx)
	latency := time.Since(start).Milliseconds()
	if err == nil {
		return &ChannelTestResult{OK: true, LatencyMS: latency}, nil
	}
	return &ChannelTestResult{
		OK:           false,
		LatencyMS:    latency,
		ErrorSummary: s.summarizeChannelErr(channel, err),
	}, nil
}

// ---- 三渠道真实发送（复用既有发送链；未配置显式报错，不静默成功） ----

// sendTestWebhook webhook 测试推送（结构化 payload，event=notify.test）。
func (s *Service) sendTestWebhook(ctx context.Context) error {
	if s.webhookTarget() == "" {
		return ErrChannelNotConfigured
	}
	body, err := json.Marshal(webhookPayload{
		Event:      "notify.test",
		Title:      channelTestTitle,
		Content:    channelTestContent,
		NotifyType: "system",
		Level:      "info",
		Timestamp:  time.Now().Unix(),
	})
	if err != nil {
		return err
	}
	return s.sendWebhook(ctx, "notify.test", body)
}

// sendTestTelegram Telegram 测试消息（sendTelegram 自带 SSRF 复验 + 凭据净化）。
func (s *Service) sendTestTelegram(ctx context.Context) error {
	cfg := s.telegram()
	if cfg == nil {
		return ErrChannelNotConfigured
	}
	return s.sendTelegram(ctx, cfg, channelTestTitle, channelTestContent)
}

// sendTestBark Bark 测试推送（sendBark 自带 SSRF 复验 + 凭据净化）。
func (s *Service) sendTestBark(ctx context.Context) error {
	cfg := s.bark()
	if cfg == nil {
		return ErrChannelNotConfigured
	}
	return s.sendBark(ctx, cfg, channelTestTitle, channelTestContent)
}

// ---- 失败摘要脱敏 ----

// summarizeChannelErr 把发送错误转成**可安全回给管理台**的摘要。
//
// 脱敏两层（作用在「回给用户」的出口；与日志侧 sanitizePushErr 同思想，
// 但那里是防凭据进日志，这里是防凭据回显给浏览器/截图/工单）：
//
//  1. *url.Error 剥 URL：错误原文是 `Post "https://api.telegram.org/bot<token>/…":
//     dial tcp …`——完整目标 URL 直接回显 = 明文凭据出网；只留 Op 与内层传输原因
//     （如 connection refused / i/o timeout，本身足够排障）。
//  2. 凭据串兜底打码：非 url.Error 路径（服务端错误体恰好回显凭据、
//     或上游某层忘了净化）按渠道配置里的凭据串逐个打码。
//
// 负对照（channeltest_test.go 实跑）：回退任一层 ⇒ 判据转红。
func (s *Service) summarizeChannelErr(channel string, err error) string {
	if errors.Is(err, ErrChannelNotConfigured) {
		return "渠道未配置或配置不完整"
	}
	msg := err.Error()
	var ue *url.Error
	if errors.As(err, &ue) {
		msg = ue.Op + ": " + ue.Err.Error()
	}
	for _, secret := range s.channelSecrets(channel) {
		if secret != "" {
			msg = strings.ReplaceAll(msg, secret, "***")
		}
	}
	return msg
}

// channelSecrets 该渠道的凭据串清单（兜底打码用）：webhook 取整个目标 URL
// （URL 常内嵌凭据路径，如 Slack/Discord webhook token），telegram 取 bot
// token，bark 取 device key。
func (s *Service) channelSecrets(channel string) []string {
	switch channel {
	case ChannelWebhook:
		if t := s.webhookTarget(); t != "" {
			return []string{t}
		}
	case ChannelTelegram:
		if cfg := s.telegram(); cfg != nil {
			return []string{cfg.BotToken}
		}
	case ChannelBark:
		if cfg := s.bark(); cfg != nil {
			return []string{cfg.DeviceKey}
		}
	}
	return nil
}
