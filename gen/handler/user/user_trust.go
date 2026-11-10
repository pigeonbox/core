package user

// 账号与信任三组端点的 hz handler（5.2.0：TOTP / 邮箱找回密码 / 登录设备）。
//
// 为什么手写在生成物目录里：hz update 对**既有** handler 文件只追加桩、不重写
// 函数体，但对**新增方法**会往既有 service 文件里塞桩（与手写实现冲突）。
// 本仓的处置与 gen/handler/admin/localfiles_ops.go 一致——生成物目录下的手写文件，
// CI 的 Gen drift check 对 gen/handler 只对账**文件增删**，不比对内容。
//
// 全部逻辑都在 app/user 里；本文件只做「取身份 → 调域服务 → 映射响应」。
// 错误映射统一走 writeTrustErr：typed 错误按 ErrCode 透传（40012/40013/40014
// 必须原样到前端，否则前端无法区分「要二验码」和「码错了」）。

import (
	"context"
	"errors"
	"strconv"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/pigeonbox/contracts/errcode"
	usermodel "github.com/pigeonbox/contracts/gen/user"
	"github.com/pigeonbox/core/pkg/resp"
)

// trustUserID 取当前登录用户 ID（AuthMiddleware 已注入；缺失即 401）。
func trustUserID(c *app.RequestContext) (uint, bool) {
	v, ok := c.Get("user_id")
	if !ok {
		return 0, false
	}
	id, ok := v.(uint)
	return id, ok
}

// trustSessionID 取当前会话 sid（5.2.0；升级前签发的老令牌 / 会话表故障时为空）。
func trustSessionID(c *app.RequestContext) string {
	if v, ok := c.Get("session_id"); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// writeTrustErr 业务错误 → wire。typed 错误按 ErrCode 透传，其余 500。
func writeTrustErr(c *app.RequestContext, err error) {
	var typed interface{ ErrCode() int }
	if errors.As(err, &typed) {
		code := typed.ErrCode()
		status := consts.StatusBadRequest
		switch code {
		case errcode.CodeUnauthorized:
			status = consts.StatusUnauthorized
		case errcode.CodeTooManyAttempts:
			status = consts.StatusTooManyRequests
		case errcode.CodeInternal:
			status = consts.StatusInternalServerError
		}
		c.JSON(status, map[string]interface{}{"code": code, "message": err.Error()})
		return
	}
	c.JSON(consts.StatusInternalServerError, map[string]interface{}{
		"code":    errcode.CodeInternal,
		"message": err.Error(),
	})
}

// TotpSetup . @router /user/totp/setup [POST]
func TotpSetup(ctx context.Context, c *app.RequestContext) {
	uid, ok := trustUserID(c)
	if !ok {
		c.JSON(consts.StatusUnauthorized, map[string]interface{}{
			"code": errcode.CodeUnauthorized, "message": "用户未登录",
		})
		return
	}
	res, err := getUserService().TotpSetup(ctx, uid)
	if err != nil {
		writeTrustErr(c, err)
		return
	}
	c.JSON(consts.StatusOK, &usermodel.TotpSetupResp{
		Code:    0,
		Message: "请用认证器扫描后输入验证码完成绑定",
		Data:    &usermodel.TotpSetupData{Secret: res.Secret, OtpauthURL: res.OtpauthURL},
	})
}

// TotpEnable . @router /user/totp/enable [POST]
func TotpEnable(ctx context.Context, c *app.RequestContext) {
	uid, ok := trustUserID(c)
	if !ok {
		c.JSON(consts.StatusUnauthorized, map[string]interface{}{
			"code": errcode.CodeUnauthorized, "message": "用户未登录",
		})
		return
	}
	var req usermodel.TotpEnableReq
	if err := c.BindAndValidate(&req); err != nil {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{
			"code": errcode.CodeInvalidParam, "message": err.Error(),
		})
		return
	}
	codes, err := getUserService().TotpEnable(ctx, uid, req.Code)
	if err != nil {
		writeTrustErr(c, err)
		return
	}
	c.JSON(consts.StatusOK, &usermodel.TotpEnableResp{
		Code:    0,
		Message: "二次验证已开启，请妥善保存恢复码（只显示这一次）",
		Data:    &usermodel.TotpEnableData{RecoveryCodes: codes},
	})
}

// TotpDisable . @router /user/totp/disable [POST]
func TotpDisable(ctx context.Context, c *app.RequestContext) {
	uid, ok := trustUserID(c)
	if !ok {
		c.JSON(consts.StatusUnauthorized, map[string]interface{}{
			"code": errcode.CodeUnauthorized, "message": "用户未登录",
		})
		return
	}
	var req usermodel.TotpDisableReq
	if err := c.BindAndValidate(&req); err != nil {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{
			"code": errcode.CodeInvalidParam, "message": err.Error(),
		})
		return
	}
	if err := getUserService().TotpDisable(ctx, uid, req.Password, req.Code); err != nil {
		writeTrustErr(c, err)
		return
	}
	c.JSON(consts.StatusOK, &usermodel.TotpDisableResp{Code: 0, Message: "二次验证已关闭"})
}

// ForgotPassword . @router /user/password/forgot [POST]
//
// ⚠️ **恒返回成功**：邮箱是否存在、SMTP 是否可用都不改变响应形态。
// 任何差异化响应都会把「这个邮箱注册过本站」卖给任意访客。
func ForgotPassword(ctx context.Context, c *app.RequestContext) {
	var req usermodel.PasswordForgotReq
	if err := c.BindAndValidate(&req); err != nil {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{
			"code": errcode.CodeInvalidParam, "message": err.Error(),
		})
		return
	}
	if err := getUserService().RequestPasswordReset(ctx, req.Email); err != nil {
		// core#119：邮箱格式非法 = 参数错误（400，全站信封）。恒成功语义只
		// 覆盖账号枚举 oracle（邮箱是否存在/SMTP/发信失败），格式非法是纯
		// 输入校验，不泄露注册状态。
		resp.NewTypedError(c, err)
		return
	}
	c.JSON(consts.StatusOK, &usermodel.PasswordForgotResp{
		Code:    0,
		Message: "若该邮箱已注册，我们已发送重置链接（请检查收件箱与垃圾邮件）",
	})
}

// ResetPassword . @router /user/password/reset [POST]
func ResetPassword(ctx context.Context, c *app.RequestContext) {
	var req usermodel.PasswordResetReq
	if err := c.BindAndValidate(&req); err != nil {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{
			"code": errcode.CodeInvalidParam, "message": err.Error(),
		})
		return
	}
	if err := getUserService().ResetPassword(ctx, req.Token, req.NewPassword); err != nil {
		writeTrustErr(c, err)
		return
	}
	c.JSON(consts.StatusOK, &usermodel.PasswordResetResp{
		Code:    0,
		Message: "密码已重置，请用新密码登录",
	})
}

// UserSessions . @router /user/sessions [GET]
func UserSessions(ctx context.Context, c *app.RequestContext) {
	uid, ok := trustUserID(c)
	if !ok {
		c.JSON(consts.StatusUnauthorized, map[string]interface{}{
			"code": errcode.CodeUnauthorized, "message": "用户未登录",
		})
		return
	}
	items, err := getUserService().ListSessions(ctx, uid, trustSessionID(c))
	if err != nil {
		writeTrustErr(c, err)
		return
	}
	out := make([]*usermodel.UserSessionItem, 0, len(items))
	for _, it := range items {
		out = append(out, &usermodel.UserSessionItem{
			ID:         int64(it.ID),
			IP:         it.IP,
			UserAgent:  it.UserAgent,
			CreatedAt:  it.CreatedAt.Format("2006-01-02 15:04:05"),
			LastSeenAt: it.LastSeenAt.Format("2006-01-02 15:04:05"),
			Current:    it.Current,
		})
	}
	c.JSON(consts.StatusOK, &usermodel.UserSessionsResp{
		Code: 0, Message: "获取成功",
		Data: &usermodel.UserSessionsData{Sessions: out},
	})
}

// RevokeSession . @router /user/sessions/:id [DELETE]
func RevokeSession(ctx context.Context, c *app.RequestContext) {
	uid, ok := trustUserID(c)
	if !ok {
		c.JSON(consts.StatusUnauthorized, map[string]interface{}{
			"code": errcode.CodeUnauthorized, "message": "用户未登录",
		})
		return
	}
	raw := c.Param("id")
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{
			"code": errcode.CodeInvalidParam, "message": "会话 ID 非法",
		})
		return
	}
	if err := getUserService().RevokeSession(ctx, uid, uint(id)); err != nil {
		writeTrustErr(c, err)
		return
	}
	c.JSON(consts.StatusOK, &usermodel.RevokeSessionResp{Code: 0, Message: "该设备已退出登录"})
}

// RevokeAllSessions . @router /user/sessions/revoke-all [POST]
//
// ⚠️ 会话纪元 +1 ⇒ **当前设备也会被踢**（这是「退出所有设备」的应有语义）。
// 前端收到 200 后必须清本地令牌并回登录页，否则下一次请求才 401 会显得像故障。
func RevokeAllSessions(ctx context.Context, c *app.RequestContext) {
	uid, ok := trustUserID(c)
	if !ok {
		c.JSON(consts.StatusUnauthorized, map[string]interface{}{
			"code": errcode.CodeUnauthorized, "message": "用户未登录",
		})
		return
	}
	n, err := getUserService().RevokeAllSessions(ctx, uid)
	if err != nil {
		writeTrustErr(c, err)
		return
	}
	c.JSON(consts.StatusOK, &usermodel.RevokeAllSessionsResp{
		Code:    0,
		Message: "已退出全部 " + strconv.FormatInt(n, 10) + " 个设备，请重新登录",
	})
}
