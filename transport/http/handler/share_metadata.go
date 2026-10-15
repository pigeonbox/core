// 取件元数据端点（GET /share/metadata/:code，手写路由）。
// 查询不扣次数、不要求密码；限流走 download 桶（2026-10-08 起未归桶路径另有全局默认桶兜底），
// 存在性探测面由 6 位随机码空间 + 统一 404 语义约束。
package handler

import (
	"context"
	"github.com/pigeonbox/contracts/errcode"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/pigeonbox/core/pkg/resp"
)

// ShareMetadata 元数据查询：供取件页渲染文件名/大小/有效期/剩余次数
func ShareMetadata(ctx context.Context, c *app.RequestContext) {
	if shareSvc == nil {
		c.JSON(consts.StatusInternalServerError, map[string]interface{}{"code": errcode.CodeInternal, "message": "share service 未就绪"})
		return
	}
	code := c.Param("code")
	if code == "" {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": errcode.CodeInvalidParam, "message": "缺少取件码"})
		return
	}
	meta, err := getShareService().GetShareMetadata(ctx, code)
	if err != nil {
		// 不区分「不存在/已过期/被封禁」，统一 404 降低探测面
		c.JSON(consts.StatusNotFound, map[string]interface{}{"code": errcode.CodeNotFound, "message": "分享不存在或已过期"})
		return
	}
	resp.Success(c, meta)
}
