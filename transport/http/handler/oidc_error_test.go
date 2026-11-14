// OIDC 回调失败必须携带**可区分**的错误标识（2026-10-26 审计 P2）。
//
// 缺陷：redirectOIDCError(c, msg) 的 msg 是**死参数**——注释声称「携带错误信息」，
// 实现却只回裸 `/#/user/login?oidc_error=1` ⇒ 用户与运维无法区分「IdP 挂了」
// 「state 过期」「账号被禁用」。
//
// 断言必须能变红（负对照）：把 Location 改回 `/#/user/login?oidc_error=1`
// （裸标识）即失败。
package handler

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	oidcApp "github.com/pigeonbox/core/app/oidc"
	"github.com/pigeonbox/core/repo/db/model"
)

// newOIDCHandlerService 构造一个「启用但必然回调失败」的 OIDC 服务：
// Enabled=true（让 handler 进入回调分支），state 校验必然失败（无 nonce Cookie）。
func newOIDCHandlerService(t *testing.T) *oidcApp.Service {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.User{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return oidcApp.NewService(g, oidcApp.Config{
		Enabled:  true,
		Issuer:   "https://idp.invalid",
		ClientID: "pb-client",
	})
}

// TestOIDCCallback_ErrorCarriesDistinguishableCode 回调失败 ⇒ 302 的 Location
// 必须携带可区分的短码与可公开文案（而非裸 oidc_error=1）。
func TestOIDCCallback_ErrorCarriesDistinguishableCode(t *testing.T) {
	SetOIDCService(newOIDCHandlerService(t))
	t.Cleanup(func() { SetOIDCService(nil) })

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.GET("/api/v1/user/oidc/callback", OIDCCallback)

	// 无 nonce Cookie ⇒ state 校验失败（回调失败路径）
	w := ut.PerformRequest(h.Engine, "GET", "/api/v1/user/oidc/callback?code=abc&state=xyz", nil,
		ut.Header{Key: "Host", Value: "test.local"})
	require.Equal(t, 302, w.Code)

	loc := w.Header().Get("Location")
	require.NotEmpty(t, loc)
	assert.NotEqual(t, "/#/user/login?oidc_error=1", loc,
		"不得回裸 oidc_error=1（用户/运维无法区分失败原因）")
	assert.Contains(t, loc, "oidc_error=1", "保持既有前端契约（前端按该参数判失败）")
	assert.Contains(t, loc, "oidc_code=state_invalid", "必须携带可区分的短码")
	assert.Contains(t, loc, "msg=", "必须携带可公开文案")

	// 文案不得泄露 IdP 内部细节
	// 回跳目标是 **hash 路由**（/#/user/login?...）⇒ query 在 fragment 里，
	// 不是 URL 的 query 段 ⇒ 要从 fragment 解析。
	q := hashRouteQuery(t, loc)
	assert.NotContains(t, q.Get("msg"), "state 校验失败",
		"不得透传原始错误文本")
	assert.Equal(t, "登录请求已过期，请重新登录", q.Get("msg"))
}

// hashRouteQuery 解析 hash 路由回跳地址里的 query（/#/path?a=b）。
func hashRouteQuery(t *testing.T, location string) url.Values {
	t.Helper()
	u, err := url.Parse(location)
	require.NoError(t, err)
	frag := u.Fragment
	idx := strings.Index(frag, "?")
	require.GreaterOrEqual(t, idx, 0, "回跳地址应带 query: %s", location)
	q, err := url.ParseQuery(frag[idx+1:])
	require.NoError(t, err)
	return q
}

// TestOIDCCallback_MissingCodeCarriesCode 缺少 code ⇒ 短码为 missing_code。
func TestOIDCCallback_MissingCodeCarriesCode(t *testing.T) {
	SetOIDCService(newOIDCHandlerService(t))
	t.Cleanup(func() { SetOIDCService(nil) })

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.GET("/api/v1/user/oidc/callback", OIDCCallback)

	w := ut.PerformRequest(h.Engine, "GET", "/api/v1/user/oidc/callback", nil,
		ut.Header{Key: "Host", Value: "test.local"})
	require.Equal(t, 302, w.Code)
	loc := w.Header().Get("Location")
	assert.Contains(t, loc, "oidc_code=missing_code")
}

// TestOIDCPublicError_Mapping 错误 → (短码, 可公开文案) 的受限映射。
//
// 关键约束：**不透传原始 err.Error()**（会泄露 IdP 内部细节：端点 URL、
// token 响应片段、账号标识）。
func TestOIDCPublicError_Mapping(t *testing.T) {
	// 账号被禁用 ⇒ 独立短码
	code, msg := oidcPublicError(oidcApp.ErrAccountDisabled)
	assert.Equal(t, "account_disabled", code)
	assert.Equal(t, "账号已被禁用", msg)

	// state 过期/伪造 ⇒ 独立短码
	code, msg = oidcPublicError(oidcApp.ErrStateInvalid)
	assert.Equal(t, "state_invalid", code)
	assert.Equal(t, "登录请求已过期，请重新登录", msg)

	// IdP email claim 格式非法（core#119 同族）⇒ 独立短码，不回显 claim 原值
	code, msg = oidcPublicError(oidcApp.ErrEmailInvalid)
	assert.Equal(t, "email_invalid", code)
	assert.Equal(t, "身份提供方返回的邮箱格式无效", msg)

	// IdP 故障 ⇒ 通用短码，且**不得**泄露内部细节
	secret := errors.New("token 交换失败: http://idp.internal/realms/pb/token 响应片段 abc123")
	code, msg = oidcPublicError(secret)
	assert.Equal(t, "login_failed", code)
	assert.Equal(t, "登录失败，请重试", msg)
	assert.NotContains(t, msg, "idp.internal")
	assert.NotContains(t, msg, "abc123")
	assert.False(t, strings.Contains(msg, secret.Error()))
}

// TestOIDCPublicError_AdminMFARequired（wave1 A1 负向测试 4）：签发拦截的
// sentinel 必须映射到专用短码 + 含「使用账号密码登录」指引的公开文案
// （回调走 redirectOIDCError 呈现；不得落到通用 login_failed 吞掉指引，
// 也不得透传原始 err 文本）。
//
// 负对照（改错转红）：删掉 oidcPublicError 的 ErrAdminMFARequired 分支 ⇒
// 落 default ⇒ code 转 login_failed、msg 转通用文案，两条断言同时转红。
func TestOIDCPublicError_AdminMFARequired(t *testing.T) {
	code, msg := oidcPublicError(oidcApp.ErrAdminMFARequired)
	assert.Equal(t, "admin_mfa_required", code,
		"已启用 MFA 的管理员被拦必须有专用短码（前端/运维可区分）")
	assert.Contains(t, msg, "使用账号密码登录",
		"文案必须给出「使用账号密码登录」指引（否则用户无路可走）")
}
