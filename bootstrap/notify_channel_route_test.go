package bootstrap

// core#122 负对照：POST /admin/notify/channels/test 未带凭据 ⇒ 401/403。
// 该端点真实发送外发消息（外发面）⇒ 必须有管理面强制鉴权；
// 2026-11-13 IDL 化后由 gen/router/admin 的组级 AdminMiddleware 提供
// （与 /admin/notify/smtp/test 同族）；若生成路由漏接线，本用例转红。

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/stretchr/testify/assert"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/gen/router"
)

func TestNotifyChannelTestRouteRequiresAdmin(t *testing.T) {
	prev := config
	config = &conf.AppConfiguration{
		MCP:      conf.MCPConfig{Enabled: true},
		Security: conf.SecurityConfig{OIDC: conf.OIDCConfig{Enabled: true}},
		UI:       conf.UIConfig{ExposeOpenAPI: true},
	}
	defer func() { config = prev }()

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	router.GeneratedRegister(h)
	customizedRegister(h)

	body := "{\"channel\":\"webhook\"}"
	w := ut.PerformRequest(h.Engine, "POST", "/admin/notify/channels/test",
		&ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	assert.True(t, w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden,
		"未带凭据必须被挡（401/403），实际 %d", w.Code)
}
