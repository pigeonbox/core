// 管理端设置测试端点：SMTP 测试发信 / OIDC discovery 验证（AdminMiddleware 组内）。
// 读取的是叠加后的全局 conf（管理端保存的在线设置已热应用），测的就是"当前生效值"。
package handler

import (
	"context"
	"errors"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	notifyApp "github.com/pigeonbox/core/app/notify"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/resp"
)

type adminTestSMTPReq struct {
	To string `json:"to"`
}

// TestSMTPNow 用当前生效的 notify.smtp 配置向指定邮箱发送测试邮件。
// 导出桥（gen handler 委托用；conf 直读面留在本白名单层）。
func TestSMTPNow(to string) error {
	smtp := conf.GetGlobalConfig().Notify.SMTP
	if smtp.Host == "" {
		return errors.New("SMTP 未配置（host 为空）")
	}
	mailer := notifyApp.NewSMTPMailer(smtp.Host, smtp.Port, smtp.Username, smtp.Password, smtp.From)
	return mailer.SendTo(strings.TrimSpace(to), "PigeonBox SMTP 测试邮件",
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
		c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": 400, "message": "需要收件邮箱 to"})
		return
	}
	if err := TestSMTPNow(req.To); err != nil {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": 400, "message": err.Error()})
		return
	}
	resp.SuccessWithMessage(c, "测试邮件已发送", nil)
}

// AdminTestOIDC POST /admin/oidc/test
// 验证当前生效 OIDC 段的 issuer discovery 端点可达且合法。
func AdminTestOIDC(ctx context.Context, c *app.RequestContext) {
	if err := TestOIDCDiscovery(ctx); err != nil {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": 400, "message": err.Error()})
		return
	}
	resp.SuccessWithMessage(c, "OIDC discovery 验证通过", nil)
}
