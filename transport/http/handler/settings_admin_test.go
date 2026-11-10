// core#119 同族收口（第五项）：AdminTestSMTP 的收件人 to 仅 TrimSpace 即拼进
// SMTP To 头——注入类输入（x@y.com\r\nBcc:evil）可外带测试邮件/注入附加头。
// 补上与注册面同款的 utils.ValidateEmail：失败 400 + 10001 + 点名 to。
//
// 判据：transport 入口 400 信封（点名 to + trace_id）；TestSMTPNow 公共闸
// （覆盖 gen handler 委托入口）在拨号前拒绝；合法 to 放行到 SMTP 配置检查。
//
// 负对照（已实跑）：删掉两处 ValidateEmail 闸 ⇒ 转红（注入输入走到 SMTP 流程）。
package handler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"

	"github.com/pigeonbox/core/conf"
)

func TestAdminTestSMTP_MalformedTo400(t *testing.T) {
	old := conf.GetGlobalConfig()
	conf.SetGlobalConfig(&conf.AppConfiguration{}) // SMTP 未配置：校验失败不得触达 SMTP 流程
	t.Cleanup(func() { conf.SetGlobalConfig(old) })
	for _, to := range []string{
		"x@y.com\r\nBcc:evil@evil.com", // CRLF 注入（SMTP To 头注入面）
		"Name <x@y.com>",               // display-name 形态
		"not-an-email",
	} {
		c := app.NewContext(0)
		body, _ := json.Marshal(map[string]string{"to": to})
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.SetBody(body)
		AdminTestSMTP(context.Background(), c)
		assertParam400(t, c, "to")
	}
}

// TestTestSMTPNow_ValidatesToBeforeDial 公共闸在拨号/配置检查前先校验：
// 非法 to ⇒ 「参数 to 非法」；合法 to ⇒ 放行到「SMTP 未配置」（证明校验只拦
// 格式，且合法输入不被误伤）。
func TestTestSMTPNow_ValidatesToBeforeDial(t *testing.T) {
	old := conf.GetGlobalConfig()
	conf.SetGlobalConfig(&conf.AppConfiguration{}) // SMTP 未配置
	t.Cleanup(func() { conf.SetGlobalConfig(old) })

	err := TestSMTPNow("x@y.com\r\nBcc:evil@evil.com")
	if err == nil || !strings.Contains(err.Error(), "to") {
		t.Fatalf("非法 to 应报「参数 to 非法」，实得 %v", err)
	}
	if !strings.Contains(err.Error(), "参数 to") {
		t.Fatalf("错误应点名 to，实得 %q", err.Error())
	}

	err = TestSMTPNow("ok@example.com")
	if err == nil || !strings.Contains(err.Error(), "SMTP 未配置") {
		t.Fatalf("合法 to 应放行到 SMTP 配置检查，实得 %v", err)
	}
}
