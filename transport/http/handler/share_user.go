// Package handler 提供定制 HTTP handlers（不走 thrift IDL 生成）。
package handler

import (
	"context"
	"github.com/pigeonbox/contracts/errcode"
	"os"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/pigeonbox/core/app/share"
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
