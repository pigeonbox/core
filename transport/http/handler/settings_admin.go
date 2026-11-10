// 管理端设置测试端点：SMTP 测试发信 / OIDC discovery 验证（AdminMiddleware 组内）。
// 读取的是叠加后的全局 conf（管理端保存的在线设置已热应用），测的就是"当前生效值"。
package handler

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/pigeonbox/contracts/errcode"

	notifyApp "github.com/pigeonbox/core/app/notify"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/resp"
	"github.com/pigeonbox/core/pkg/utils"
)

type adminTestSMTPReq struct {
	To string `json:"to"`
}

// TestSMTPNow 用当前生效的 notify.smtp 配置向指定邮箱发送测试邮件。
// 导出桥（gen handler 委托用；conf 直读面留在本白名单层）。
func TestSMTPNow(to string) error {
	to = strings.TrimSpace(to)
	// core#119 同族收口：to 仅 TrimSpace 即拼进 SMTP To 头 ⇒ 与注册面同款
	// utils.ValidateEmail 严格校验，注入类输入在拨号前拒绝。admin-only 自害面，
	// 但校验口径全仓一致。本函数是两个入口（transport AdminTestSMTP 与
	// gen handler 委托）的公共闸。
	if err := utils.ValidateEmail(to); err != nil {
		return fmt.Errorf("参数 to 非法：%s", err.Error())
	}
	smtp := conf.GetGlobalConfig().Notify.SMTP
	if smtp.Host == "" {
		return errors.New("SMTP 未配置（host 为空）")
	}
	mailer := notifyApp.NewSMTPMailer(smtp.Host, smtp.Port, smtp.Username, smtp.Password, smtp.From, nil) // 仅 SendTo 直发，无需用户邮箱查询
	return mailer.SendTo(to, "PigeonBox SMTP 测试邮件",
		"这是一封来自 PigeonBox 的测试邮件，收到即代表 SMTP 配置生效。")
}

// TestOIDCDiscovery 验证当前生效 OIDC 段的 issuer discovery 端点可达且合法。
func TestOIDCDiscovery(ctx context.Context) error {
	svc := getOIDCService()
	if svc == nil {
		return errors.New("OIDC 未配置")
	}
	return svc.TestDiscovery(ctx)
}

// AdminTestSMTP POST /admin/notify/smtp/test {"to":"you@example.com"}
// 用当前生效的 notify.smtp 配置向指定邮箱发送测试邮件。
func AdminTestSMTP(ctx context.Context, c *app.RequestContext) {
	var req adminTestSMTPReq
	if err := c.BindJSON(&req); err != nil || strings.TrimSpace(req.To) == "" {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": errcode.CodeInvalidParam, "message": "需要收件邮箱 to"})
		return
	}
	// core#119 同族收口：to 直接进 SMTP To 头 ⇒ 严格校验，失败 400 + 10001 +
	// 点名 to（全站信封）。TestSMTPNow 内另有同款公共闸（覆盖 gen 委托入口）。
	to := strings.TrimSpace(req.To)
	if err := utils.ValidateEmail(to); err != nil {
		resp.NewErrorWithMessage(c, errcode.CodeInvalidParam, fmt.Sprintf("参数 to 非法：%s", err.Error()))
		return
	}
	if err := TestSMTPNow(to); err != nil {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": errcode.CodeInvalidParam, "message": err.Error()})
		return
	}
	resp.SuccessWithMessage(c, "测试邮件已发送", nil)
}

// AdminTestOIDC POST /admin/oidc/test
// 验证当前生效 OIDC 段的 issuer discovery 端点可达且合法。
func AdminTestOIDC(ctx context.Context, c *app.RequestContext) {
	if err := TestOIDCDiscovery(ctx); err != nil {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": errcode.CodeInvalidParam, "message": err.Error()})
		return
	}
	resp.SuccessWithMessage(c, "OIDC discovery 验证通过", nil)
}
