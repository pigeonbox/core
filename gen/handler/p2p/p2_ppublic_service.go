// p2_ppublic_service.go — 实例间推送邀请收件端点（P3，spec §14）。
// hz 首生成桩后转手写（模型引 contracts/gen/p2p）。公开无鉴权（口令即
// bearer——邀请体只有口令，资产即权限），归 download 限流桶；入箱幂等去重。
package p2p

import (
	"context"
	"github.com/pigeonbox/contracts/errcode"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	p2p "github.com/pigeonbox/contracts/gen/p2p"
)

// inviteNotifier 邀请到达通知（notify 域 webhook 事件；wiring 注入，nil=跳过）。
var inviteNotifier func(fromName, code, note string)

// SetInviteNotifier 注入邀请到达通知器（bootstrap/wiring 调用）。
func SetInviteNotifier(fn func(fromName, code, note string)) { inviteNotifier = fn }

// PostP2PInvite 接收来自其他实例的直传邀请。
// @router /api/v1/p2p/invite [POST]
func PostP2PInvite(ctx context.Context, c *app.RequestContext) {
	var req p2p.PostP2PInviteReq
	if err := c.BindAndValidate(&req); err != nil {
		c.JSON(consts.StatusBadRequest, &p2p.PostP2PInviteResp{Code: errcode.CodeInvalidParam, Message: "请求体无效"})
		return
	}
	inv, err := getP2px().AddInvite(req.GetCode(), req.GetFromName(), req.GetNote())
	if err != nil {
		c.JSON(consts.StatusBadRequest, &p2p.PostP2PInviteResp{Code: errcode.CodeInvalidParam, Message: err.Error()})
		return
	}
	if inviteNotifier != nil {
		inviteNotifier(inv.FromName, inv.Code, inv.Note) // best-effort,失败不影响收件
	}
	c.JSON(consts.StatusOK, &p2p.PostP2PInviteResp{Code: 0, Message: "邀请已送达"})
}
