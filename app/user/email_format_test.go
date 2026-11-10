package user

// core#119：邮箱写入的格式收口（SMTP 头注入面）。
//
// 邮箱进 SMTP 信封头（notify.buildMessage 的 To/From 字符串拼接），
// 注册/建号与找回入口必须在输入层拒绝 CRLF 等注入类输入。
//
// 负对照（已实跑）：
//   - 删掉 Service.Create 里的 utils.ValidateEmail 分支 ⇒
//     TestCreate_RejectsMalformedEmail 转红（用户被建出来）；
//   - 删掉 RequestPasswordReset 里的校验分支 ⇒
//     TestRequestPasswordReset_MalformedEmailRejected 转红（恒成功吞掉）。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgerrors "github.com/pigeonbox/core/pkg/errors"
)

// TestCreate_RejectsMalformedEmail 注册/建号拒绝注入类与 display-name 邮箱。
func TestCreate_RejectsMalformedEmail(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	_, err := svc.Create(ctx, &CreateUserReq{
		Username: "mallory", Email: "x@y.com\r\nBcc:evil@evil.com", Password: "x",
	})
	require.Error(t, err, "CRLF 注入类邮箱必须拒绝")
	var appErr *pkgerrors.AppError
	require.ErrorAs(t, err, &appErr, "必须是 typed 参数错误（400 面），不是 500")
	assert.Equal(t, pkgerrors.CodeBadRequest, appErr.BizCode)

	_, err = svc.repo.GetByUsername(ctx, "mallory")
	assert.Error(t, err, "非法邮箱不得建号（负对照：删掉校验 ⇒ 此处转绿=用户被建出来）")

	// display-name 形态同样拒绝（地址字面量全等判据）
	_, err = svc.Create(ctx, &CreateUserReq{
		Username: "mallory2", Email: "Name <x@y.com>", Password: "x",
	})
	var appErr2 *pkgerrors.AppError
	require.ErrorAs(t, err, &appErr2)

	// 合法邮箱照常建号（别把好输入一起挡了）
	_, err = svc.Create(ctx, &CreateUserReq{
		Username: "gooduser", Email: "good@example.com", Password: "x",
	})
	require.NoError(t, err)
}

// TestCreate_EmptyEmailUnchanged 空邮箱保持既有口径：管理端建号允许不填。
func TestCreate_EmptyEmailUnchanged(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.Create(context.Background(), &CreateUserReq{
		Username: "noemail", Email: "", Password: "x",
	})
	require.NoError(t, err)
}

// TestRequestPasswordReset_MalformedEmailRejected 找回入口同款收口；
// 合法格式（含未知邮箱）维持恒成功（防账号枚举语义不动）。
func TestRequestPasswordReset_MalformedEmailRejected(t *testing.T) {
	svc, _ := newTrustService(t)
	ctx := context.Background()

	err := svc.RequestPasswordReset(ctx, "x@y.com\r\nBcc:evil@evil.com")
	var appErr *pkgerrors.AppError
	require.ErrorAs(t, err, &appErr, "注入类邮箱必须按参数错误上抛（不是恒成功吞掉）")
	assert.Equal(t, pkgerrors.CodeBadRequest, appErr.BizCode)

	require.NoError(t, svc.RequestPasswordReset(ctx, "nobody@example.com"),
		"合法格式的未知邮箱必须恒成功（防账号枚举）")
	require.NoError(t, svc.RequestPasswordReset(ctx, ""), "空邮箱维持恒成功")
}
