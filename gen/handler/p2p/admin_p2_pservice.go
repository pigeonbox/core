// gen/handler/p2p/admin_p2_pservice.go — P2P 联邦管理面（2026-10-22 Wave B'：
// 设备直传面整体拆除后，本文件只剩联邦四端点）。
//
// hz 首生成桩后本文件转手写（模型引 contracts/gen/p2p，hzmodel 临时目录不落地）。
// 全部端点挂 /admin 组（AdminMiddleware），操作留审计；本层只做编排，
// 不代理文件流量。
//
// 2026-10-22 两应用分拆（Wave B'）：设备直传（p2pc / p2pcweb）已拆为独立应用
// **PigeonBeam（信鸽直传）**，Box 侧不再编排直传 —— 直传九端点
// （status / tasks / tasks/:id/log / send / send/upload / recv / cancel /
// tasks/:id/approve / publish）与直传任务模型整体退役。
//
// 联邦实现**不再寄居直传包**：此前 invites / invite-push / directory 三个
// 纯联邦端点调 getP2px()，把「联邦」藏在「直传」包里 —— 删直传包会连带
// 删联邦（这正是两应用分拆稿 Wave B 的隐藏前提）。现在四个联邦端点直接
// 取联邦域实例（app/federation），联邦能力与直传彻底解耦。
//
// 最后一处运行期焊死（invites/accept 的第二步 spawn p2pc 收件）已解开：
// 认领退化为**纯信封动作** —— 出箱 + 留痕 + 返回信封；实际文件接收由
// PigeonBeam 客户端完成（NAS 随包内嵌的 Beam 可经本 API 取走信封）。
package p2p

import (
	"context"
	"time"

	"github.com/pigeonbox/contracts/errcode"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	p2p "github.com/pigeonbox/contracts/gen/p2p"
	federation "github.com/pigeonbox/core/app/federation"
	"github.com/pigeonbox/core/pkg/logger"
)

// 装配注入（wiring 调用；与 admin gen handler 同款 fail-fast 模式）。
var (
	federationMailbox   *federation.InviteMailbox
	federationDirectory *federation.PeerDirectory
)

// SetFederationServices 注入联邦收件箱与名录（bootstrap/wiring 调用）。
func SetFederationServices(mailbox *federation.InviteMailbox, directory *federation.PeerDirectory) {
	federationMailbox = mailbox
	federationDirectory = directory
}

func getFederationMailbox() *federation.InviteMailbox {
	if federationMailbox == nil {
		panic("federation mailbox not initialized: SetFederationServices must be called before serving")
	}
	return federationMailbox
}

func getFederationDirectory() *federation.PeerDirectory {
	if federationDirectory == nil {
		panic("federation directory not initialized: SetFederationServices must be called before serving")
	}
	return federationDirectory
}

// ---- 工具 ----

func badReq(c *app.RequestContext, msg string) {
	c.JSON(consts.StatusBadRequest, map[string]any{"code": errcode.CodeInvalidParam, "message": msg})
}

func okData(c *app.RequestContext, data map[string]any) {
	c.JSON(consts.StatusOK, map[string]any{"code": 0, "message": "success", "data": data})
}

// okDataMsg 同 okData，但自定义 message（认领信封要提示接收方改用 PigeonBeam）。
func okDataMsg(c *app.RequestContext, msg string, data map[string]any) {
	c.JSON(consts.StatusOK, map[string]any{"code": 0, "message": msg, "data": data})
}

// 审计（与 admin 域同表 admin_operation_logs；wiring 注入写者，nil=跳过）。
var auditWriter func(ctx context.Context, action, target string, success bool)

// SetAuditWriter 注入审计写者（wiring 装配）。
func SetAuditWriter(fn func(ctx context.Context, action, target string, success bool)) {
	auditWriter = fn
}

// audit 操作审计。
func audit(ctx context.Context, action, target string, err error) {
	if auditWriter != nil {
		auditWriter(ctx, action, target, err == nil)
	}
}

// ---- 端点 ----

// AdminP2pInvites 待收邀请列表（联邦信封箱）。
// @router /admin/p2p/invites [GET]
func AdminP2pInvites(ctx context.Context, c *app.RequestContext) {
	invites := getFederationMailbox().ListInvites()
	items := make([]*p2p.InviteInfo, 0, len(invites))
	for _, iv := range invites {
		items = append(items, &p2p.InviteInfo{
			ID:         iv.ID,
			Code:       iv.Code,
			FromName:   iv.FromName,
			Note:       iv.Note,
			ReceivedAt: iv.ReceivedAt.Format(time.RFC3339),
			ExpiresIn:  iv.ExpiresIn,
		})
	}
	okData(c, map[string]any{"invites": items})
}

// AdminP2pInviteAccept 认领邀请（**纯信封**：出箱 + 留痕 + 返回信封）。
//
// Wave B' 前的第二步是 spawn p2pc 收件 —— 那是「联邦」与「直传」在运行期
// 焊死的最后一处。Box 侧拆除直传编排后，认领不再发起任何直传运行时：
// 文件接收由 PigeonBeam 客户端完成（NAS 随包内嵌的 Beam 经本 API 取信封）。
// @router /admin/p2p/invites/accept [POST]
func AdminP2pInviteAccept(ctx context.Context, c *app.RequestContext) {
	var req p2p.AdminP2pInviteAcceptReq
	if err := c.BindAndValidate(&req); err != nil || req.GetInviteID() == "" {
		badReq(c, "请求体无效（需 invite_id）")
		return
	}
	inv, err := getFederationMailbox().TakeInvite(req.GetInviteID())
	if err != nil {
		audit(ctx, "p2p.invite.accept", req.GetInviteID(), err)
		badReq(c, err.Error())
		return
	}
	audit(ctx, "p2p.invite.accept", inv.ID, nil)
	okDataMsg(c, "已认领邀请（信封已出箱；直传接收请用 PigeonBeam 客户端完成）", map[string]any{
		"invite": &p2p.InviteInfo{
			ID:         inv.ID,
			Code:       inv.Code,
			FromName:   inv.FromName,
			Note:       inv.Note,
			ReceivedAt: inv.ReceivedAt.Format(time.RFC3339),
			ExpiresIn:  getFederationMailbox().ExpiresIn(inv),
		},
	})
}

// AdminP2pInvitePush 把口令推送到目标实例（免口令交接）。
// @router /admin/p2p/invite-push [POST]
func AdminP2pInvitePush(ctx context.Context, c *app.RequestContext) {
	var req p2p.AdminP2pInvitePushReq
	if err := c.BindAndValidate(&req); err != nil || req.GetCode() == "" || req.GetTargetURL() == "" {
		badReq(c, "请求体无效（需 code 与 target_url）")
		return
	}
	err := federation.PushInvite(ctx, req.GetTargetURL(), req.GetCode(), req.GetFromName(), req.GetNote())
	audit(ctx, "p2p.invite.push", req.GetTargetURL(), err)
	if err != nil {
		badReq(c, err.Error())
		return
	}
	okData(c, map[string]any{"target_url": req.GetTargetURL()})
}

// AdminP2pDirectory 在线名录透传（联邦节点名录：发送卡对端选择器消费；
// p2pd 名录零敏感，本层不改写）。
// @router /admin/p2p/directory [GET]
func AdminP2pDirectory(ctx context.Context, c *app.RequestContext) {
	peers, err := getFederationDirectory().Peers(ctx)
	if err != nil {
		// 上游是外部依赖（注册中心）而非本服务内部故障：映射 502 并收敛文案。
		// 此前裸 500 + err.Error 直出（可能带上游 URL/网络细节），前端又静默
		// 渲染空下拉，admin 无法区分「没有对端」和「名录服务不可用」。
		logger.CtxError(ctx, "p2p directory upstream query failed", "err", err)
		c.JSON(consts.StatusBadGateway, map[string]any{
			"code":    errcode.CodeInternal,
			"message": "名录暂时不可用（注册中心波动），请稍后重试",
		})
		return
	}
	out := make([]*p2p.P2pDirectoryPeer, 0, len(peers))
	for _, p := range peers {
		out = append(out, &p2p.P2pDirectoryPeer{
			NodeID:      p.NodeID,
			Name:        p.Name,
			URL:         p.URL,
			BoxPub:      p.BoxPub,
			OnlineSince: p.OnlineSince,
		})
	}
	okData(c, map[string]any{"peers": out})
}
