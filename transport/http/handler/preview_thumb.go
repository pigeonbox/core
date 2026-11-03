package handler

// 缩略图取件面（GET /preview/:code/thumb，custom 路由）。
//
// 背景：预览生成器把缩略图落盘后，PreviewData.Thumbnail 直出服务器文件
// 系统路径——契约字段语义是「缩略图URL」，实际不可取且泄漏路径，缩略图
// 零消费。本面按 code 走与 /preview/:code 同款取件闸门后回缓存缩略图，
// 响应面的 Thumbnail 由 app/preview 统一改写为本路由 URL。
//
// 走 custom 路由而非 IDL：响应是二进制文件流（非 JSON 契约面），且改
// contracts IDL 需抬钉版发版；与 /qrcode 等二进制公开面同列。

import (
	"context"
	"errors"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/pigeonbox/contracts/errcode"
	previewapp "github.com/pigeonbox/core/app/preview"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/pkg/resp"
)

var previewThumbSvc *previewapp.Service

// SetPreviewService 注入预览域服务（bootstrap 与 gen/handler/preview 同实例）。
func SetPreviewService(s *previewapp.Service) {
	previewThumbSvc = s
}

// PreviewThumb 缩略图取件。闸门语义与 GET /preview/:code 完全同链
// （登录/过期/管控/密码/防爆破），密码经 X-Share-Password 头。
func PreviewThumb(ctx context.Context, c *app.RequestContext) {
	code := c.Param("code")
	if code == "" {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{
			"code":    errcode.CodeInvalidParam,
			"message": "请提供分享码",
		})
		return
	}
	if previewThumbSvc == nil {
		resp.NewErrorByCode(c, errcode.CodeUnavailable)
		return
	}

	var userID *uint
	if v, ok := middleware.UserIDFromContext(ctx); ok {
		userID = &v
	}
	path, err := previewThumbSvc.GetThumb(ctx, previewapp.GetReq{
		Code:     code,
		UserID:   userID,
		ClientIP: middleware.ClientIP(c),
		Password: string(c.GetHeader("X-Share-Password")),
	})
	if err != nil {
		var locked *previewapp.LockedError
		switch {
		case errors.As(err, &locked):
			c.JSON(consts.StatusTooManyRequests, map[string]interface{}{
				"code":    errcode.CodeTooManyAttempts,
				"message": "尝试过于频繁，已临时锁定，请稍后重试",
			})
		case errors.Is(err, previewapp.ErrShareNotReady):
			resp.NewErrorWithMessage(c, 20001, "分享未完成上传，暂时无法取件")
		case errors.Is(err, previewapp.ErrShareNotFound):
			c.JSON(consts.StatusNotFound, map[string]interface{}{
				"code":    errcode.CodeNotFound,
				"message": "分享不存在或已过期",
			})
		case errors.Is(err, previewapp.ErrPickupExhausted):
			c.JSON(consts.StatusForbidden, map[string]interface{}{
				"code":    errcode.CodeShareReachLimit,
				"message": "超过取件次数",
			})
		case errors.Is(err, previewapp.ErrShareBlocked):
			resp.NewErrorByCode(c, errcode.CodeShareBlocked)
		case errors.Is(err, previewapp.ErrPasswordRequired):
			c.JSON(consts.StatusUnauthorized, map[string]interface{}{
				"code":    errcode.CodeSharePasswordWrong,
				"message": "需要密码",
				"data":    map[string]interface{}{"has_password": true},
			})
		case errors.Is(err, previewapp.ErrPreviewUnavailable):
			// 无缩略图/缓存缺失：与预览不可用同口径 404，不区分降低探测面
			c.JSON(consts.StatusNotFound, map[string]interface{}{
				"code":    errcode.CodeNotFound,
				"message": "预览不可用",
			})
		default:
			resp.NewTypedError(c, err)
		}
		return
	}

	// 回缓存缩略图文件（Hertz 按扩展名带 Content-Type）
	c.File(path)
}
