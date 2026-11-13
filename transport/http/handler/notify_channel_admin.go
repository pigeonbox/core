// 管理端通知渠道测试发送（core#122）：POST /admin/notify/channels/test
// body {"channel":"webhook|telegram|bark"}。
//
// 与 settings_admin.go 的 SMTP/OIDC 测试端点同族（「配完先试一条」反馈回路），
// 但发送链在 app/notify（webhook/telegram/bark 真实 sender 复用 + 摘要脱敏）。
// 鉴权 = 管理面强制鉴权（2026-11-13 IDL 化：路由由 gen/router/admin 生成，
// 组级 AdminMiddleware——与同族 /admin/notify/smtp/test、/admin/oidc/test 同款；
// 手写时代的 AdminOrAdminKey 注册已摘除，收编注记见 bootstrap/routes_custom.go）。
//
// 响应约定（D2 管理台按钮消费）：
//   - 渠道发送成功/失败都是 HTTP 200 + 全站信封，data = {ok, latency_ms,
//     error_summary}（发送失败是测试的**结论**，不是 API 错误）；
//   - 仅请求本身非法（缺 channel / 未知渠道 / 非法 JSON）⇒ 400 + 10001。
package handler

import (
	"context"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/pigeonbox/contracts/errcode"

	notifyapp "github.com/pigeonbox/core/app/notify"
	"github.com/pigeonbox/core/pkg/resp"
)

type adminTestChannelReq struct {
	Channel string `json:"channel"`
}

// AdminTestNotifyChannel 向指定推送渠道真实发送一条固定测试消息。
func AdminTestNotifyChannel(ctx context.Context, c *app.RequestContext) {
	var req adminTestChannelReq
	if err := c.BindJSON(&req); err != nil {
		resp.NewErrorWithMessage(c, errcode.CodeInvalidParam, "需要 channel（webhook/telegram/bark）")
		return
	}
	channel := strings.TrimSpace(req.Channel)
	if channel == "" {
		resp.NewErrorWithMessage(c, errcode.CodeInvalidParam, "需要 channel（webhook/telegram/bark）")
		return
	}
	result, err := getNotifyService().TestChannel(ctx, channel)
	if err != nil {
		// 唯一的 error 出口 = 未知渠道（发送失败走 200 + ok=false，见文件头）
		resp.NewErrorWithMessage(c, errcode.CodeInvalidParam, "channel 必须是 webhook/telegram/bark")
		return
	}
	resp.Success(c, result)
}

// 确保与 app/notify 的结果类型同源（wire 形状单一真相源在 app/notify）。
var _ = notifyapp.ChannelTestResult{}
