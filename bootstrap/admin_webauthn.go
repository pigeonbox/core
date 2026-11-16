package bootstrap

// 管理员 Passkey 四端点（core#160，手写路由——contracts 冻结不进 IDL）：
//
//	POST /admin/webauthn/register/options  登记挑战（管理员 JWT）
//	POST /admin/webauthn/register/verify   登记校验（管理员 JWT）
//	POST /admin/webauthn/login/options     登录挑战（公开面）
//	POST /admin/webauthn/login/verify      登录校验（公开面，签发管理员 JWT）
//
// 鉴权接线在 bootstrap/routes_custom.go（register 组挂 AdminMiddleware，
// login 组挂 login 限流桶）；本文件只做参数搬运、RP 解析与错误映射。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/pigeonbox/contracts/errcode"

	adminsvc "github.com/pigeonbox/core/app/admin"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/auth"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/pkg/resp"
)

// adminWebAuthnService 包级句柄（惰性单例；bootstrap 是单例的唯一合法接线面
// ——internal/arch 规则 7；同包测试可直接换注入内存库实例）。
var adminWebAuthnService *adminsvc.Service

func getAdminWebAuthnService() *adminsvc.Service {
	if adminWebAuthnService == nil {
		adminWebAuthnService = adminsvc.Default()
	}
	return adminWebAuthnService
}

// adminWebAuthnRP 从生效配置 + 请求头解析 RP 参数。
//
// scheme 固定 http 起步（与 bootstrap.publicBaseMiddleware 同口径）：
// TLS 在反代终结时靠 X-Forwarded-Proto 与浏览器 Origin 头纠正。
func adminWebAuthnRP(c *app.RequestContext) adminsvc.WebAuthnRP {
	var cfg conf.WebAuthnConfig
	if snap := conf.Snapshot(); snap != nil {
		cfg = snap.Security.WebAuthn
	}
	return adminsvc.ResolveWebAuthnRP(
		cfg,
		string(c.Host()),
		"http",
		string(c.GetHeader("X-Forwarded-Proto")),
		string(c.GetHeader("Origin")),
	)
}

// adminWebAuthnError 统一错误映射：
// 40012=流程信号（口令已对只缺第二步，不计失败）；400=挑战/断言问题；
// 其余内部故障走脱敏出网（core#114 A6b）。
func adminWebAuthnError(c *app.RequestContext, op string, err error) {
	switch {
	case errors.Is(err, adminsvc.ErrWebAuthnRequired):
		resp.NewErrorWithMessage(c, errcode.CodeTotpRequired, err.Error())
	case errors.Is(err, adminsvc.ErrWebAuthnInvalid):
		resp.NewErrorWithMessage(c, errcode.CodeInvalidParam, err.Error())
	default:
		resp.NewInternalError(c, errcode.CodeInternal, op, err)
	}
}

// adminWebAuthnVerifyReq 登记/登录校验的公共请求体。
// challenge 由客户端回传（options 响应里拿到），服务端据此一次性取回 session；
// credential 是浏览器应答的 PublicKeyCredential 原样 JSON（库解析）。
type adminWebAuthnVerifyReq struct {
	Challenge  string          `json:"challenge"`
	Credential json.RawMessage `json:"credential"`
	Name       string          `json:"name,omitempty"` // 仅登记：passkey 别名
}

func decodeAdminWebAuthnVerifyReq(c *app.RequestContext, req *adminWebAuthnVerifyReq) bool {
	body, err := c.Body()
	if err != nil || json.Unmarshal(body, req) != nil || strings.TrimSpace(req.Challenge) == "" || len(req.Credential) == 0 {
		resp.NewErrorWithMessage(c, errcode.CodeInvalidParam, "请求参数错误: challenge/credential 必填")
		return false
	}
	return true
}

// AdminWebAuthnRegisterOptions POST /admin/webauthn/register/options。
func adminWebAuthnRegisterOptions(ctx context.Context, c *app.RequestContext) {
	uid, ok := middleware.UserIDFromContext(ctx)
	if !ok || uid == 0 {
		resp.NewErrorByCode(c, errcode.CodeUnauthorized)
		return
	}
	out, err := getAdminWebAuthnService().WebAuthnRegisterOptions(ctx, uid, adminWebAuthnRP(c))
	if err != nil {
		adminWebAuthnError(c, "Passkey 登记", err)
		return
	}
	resp.Success(c, out)
}

// AdminWebAuthnRegisterVerify POST /admin/webauthn/register/verify。
func adminWebAuthnRegisterVerify(ctx context.Context, c *app.RequestContext) {
	uid, ok := middleware.UserIDFromContext(ctx)
	if !ok || uid == 0 {
		resp.NewErrorByCode(c, errcode.CodeUnauthorized)
		return
	}
	var req adminWebAuthnVerifyReq
	if !decodeAdminWebAuthnVerifyReq(c, &req) {
		return
	}
	row, err := getAdminWebAuthnService().WebAuthnRegisterVerify(ctx, uid, adminWebAuthnRP(c), req.Challenge, req.Credential, req.Name)
	if err != nil {
		adminWebAuthnError(c, "Passkey 登记", err)
		return
	}
	resp.Success(c, row)
}

// AdminWebAuthnLoginOptions POST /admin/webauthn/login/options（公开面）。
func adminWebAuthnLoginOptions(ctx context.Context, c *app.RequestContext) {
	var req struct {
		Username string `json:"username"`
	}
	body, err := c.Body()
	if err != nil || json.Unmarshal(body, &req) != nil || strings.TrimSpace(req.Username) == "" {
		resp.NewErrorWithMessage(c, errcode.CodeInvalidParam, "请求参数错误: username 必填")
		return
	}
	out, err := getAdminWebAuthnService().WebAuthnLoginOptions(ctx, req.Username, adminWebAuthnRP(c))
	if err != nil {
		adminWebAuthnError(c, "Passkey 登录", err)
		return
	}
	resp.Success(c, out)
}

// AdminWebAuthnLoginVerify POST /admin/webauthn/login/verify（公开面）。
//
// 成功签发与 /admin/login 同形的管理员 JWT（token/token_type/expires_in）+
// HttpOnly 会话 Cookie——浏览器与脚本客户端两条通道都与口令登录一致。
func adminWebAuthnLoginVerify(ctx context.Context, c *app.RequestContext) {
	var req adminWebAuthnVerifyReq
	if !decodeAdminWebAuthnVerifyReq(c, &req) {
		return
	}
	token, err := getAdminWebAuthnService().WebAuthnLoginVerify(ctx, adminWebAuthnRP(c), req.Challenge, req.Credential)
	if err != nil {
		adminWebAuthnError(c, "Passkey 登录", err)
		return
	}
	middleware.SetSessionCookie(c, token, int(auth.SessionExpiry().Seconds()))
	resp.Success(c, map[string]interface{}{
		"token":      token,
		"token_type": "Bearer",
		"expires_in": 24 * 60 * 60, // 与 /admin/login 的 AdminLoginData 同值
	})
}
