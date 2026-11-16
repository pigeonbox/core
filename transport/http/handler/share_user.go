// Package handler 提供定制 HTTP handlers（不走 thrift IDL 生成）。
package handler

import (
	"context"
	"errors"
	"os"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/pigeonbox/contracts/errcode"

	"github.com/pigeonbox/core/app/share"
	"github.com/pigeonbox/core/pkg/resp"
	"github.com/pigeonbox/core/transport/http/middleware"
)

var shareSvc *share.Service

// SetShareService 注入 share service（bootstrap 调用）
func SetShareService(s *share.Service) {
	shareSvc = s
}

func getShareService() *share.Service {
	if shareSvc == nil {
		// fail-fast：残废实例（空 baseURL/nil storage）只会静默放大错误；
		// bootstrap 保证 SetShareService 先于服务流量，装配缺失必须显式暴露
		panic("share service not initialized: SetShareService must be called before serving")
	}
	return shareSvc
}

// userIDFromCtx 提取 userID，未登录返回 0
func userIDFromCtx(c *app.RequestContext) (uint, bool) {
	v, ok := c.Get(middleware.ContextKeyUserID)
	if !ok {
		return 0, false
	}
	id, ok := v.(uint)
	return id, ok
}

// PickupTimeline 取件时间线（owner 侧「分享详情」；frontend-core#93）。
// GET /api/v1/user/shares/:code/timeline
//
// 三态（state）：never 从未被访问 / failed 被尝试但失败 / picked 成功取件；
// events 按时间倒序合并成功（pickup_charges）与失败（pickup_failures）事件。
//
// 为什么是手写面而不是 IDL：contracts 仓本 PR 不许动、且不存在既有的
// 「未锁形」per-share 响应扩展面 ⇒ 按 routes_classification 的 Pending 豁免
// 登记为待收敛债务（wire 形态 {code,message,data}，非分页、不进 page_forms）。
func PickupTimeline(ctx context.Context, c *app.RequestContext) {
	uid, ok := userIDFromCtx(c)
	if !ok {
		resp.NewErrorWithMessage(c, errcode.CodeUnauthorized, "未登录")
		return
	}
	code := c.Param("code")
	if code == "" {
		resp.NewErrorWithMessage(c, errcode.CodeInvalidParam, "code 必填")
		return
	}
	timeline, err := getShareService().PickupTimeline(ctx, uid, code)
	if err != nil {
		// 不存在 / 不是你的 / 已删除：统一 404 同文案（防枚举）。
		if errors.Is(err, share.ErrTimelineNotFound) {
			resp.NewErrorWithMessage(c, errcode.CodeNotFound, err.Error())
			return
		}
		resp.NewInternalError(c, errcode.CodeInternal, "获取取件时间线", err)
		return
	}
	c.JSON(consts.StatusOK, map[string]interface{}{
		"code": 0, "message": "ok", "data": timeline,
	})
}

// 用户分享管理 5 端点（列表/批量删除/批量延期/恢复/硬删）已于 2026-10-09 IDL 化：
// idl/share.thrift UserShares*，handler 迁至 core/gen/handler/share/share_service.go，
// 路由由 gen/router/share 注册并挂 UserOrAPIKey（wire 形态逐字段兼容，此处摘除）。

// OpenAPISpec 返回 OpenAPI 3.0 规范
// GET /openapi.json
// 主路径：bootstrap 在路由注册完成后由运行时路由表生成（openapi_gen.go），
// 与实际注册路由零漂移。文件探测仅作兜底（./static/openapi.json = server
// 镜像布局；保留以兼容外置 spec 的部署方式）。
func OpenAPISpec(_ context.Context, c *app.RequestContext) {
	if len(openapiSpecBytes) > 0 {
		c.Data(consts.StatusOK, "application/json", openapiSpecBytes)
		return
	}
	candidates := []string{
		"./static/openapi.json",
		"./docs/openapi.json",
		"docs/openapi.json",
	}
	for _, p := range candidates {
		if data, err := os.ReadFile(p); err == nil {
			c.Data(consts.StatusOK, "application/json", data)
			return
		}
	}
	c.JSON(consts.StatusNotFound, map[string]interface{}{
		"code": errcode.CodeNotFound, "message": "openapi.json not found",
	})
}
