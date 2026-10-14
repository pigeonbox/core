// p2_ppublic_service.go — 实例间推送邀请收件端点（P3，spec §14）。
// hz 首生成桩后转手写（模型引 contracts/gen/p2p）。公开无鉴权（口令即
// bearer——邀请体只有口令，资产即权限），归 download 限流桶；入箱幂等去重。
package p2p

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	p2p "github.com/pigeonbox/contracts/gen/p2p"
)

// PostP2PInvite 接收来自其他实例的直传邀请。
// @router /api/v1/p2p/invite [POST]
func PostP2PInvite(ctx context.Context, c *app.RequestContext) {
	var req p2p.PostP2PInviteReq
	if err := c.BindAndValidate(&req); err != nil {
		c.JSON(consts.StatusBadRequest, &p2p.PostP2PInviteResp{Code: 400, Message: "请求体无效"})
		return
	}
	if _, err := getP2px().AddInvite(req.GetCode(), req.GetFromName(), req.GetNote()); err != nil {
		c.JSON(consts.StatusBadRequest, &p2p.PostP2PInviteResp{Code: 400, Message: err.Error()})
		return
	}
	c.JSON(consts.StatusOK, &p2p.PostP2PInviteResp{Code: 200, Message: "邀请已送达"})
}
