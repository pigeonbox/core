// invite.go — 联邦信封管理在直传包侧的兼容门面（2026-10-22，Wave B B0 波）。
//
// 实现已搬至 app/federation（信封属联邦域，不属直传域）：本文件只保留同名
// 委托面，使生成物（gen/handler/p2p/*）、信箱 watcher（watcher.go）与 MCP
// 适配器（bootstrap/adapters.go）零改动。
//
// 依赖方向 p2px → federation：Wave B 删掉直传编排层时，本门面连同生成物里的
// 调用点一并改造，联邦信封实现原样留存。
package p2px

import (
	"context"
	"errors"

	"github.com/pigeonbox/core/app/federation"
)

// Invite 联邦信封（类型别名 = federation.Invite：结构同形，调用方零改动）。
type Invite = federation.Invite

// InviteSnapshot 联邦信封对外快照（类型别名 = federation.InviteSnapshot）。
type InviteSnapshot = federation.InviteSnapshot

// AddInvite 邀请入箱（委托联邦信封管理；格式校验 + 同码去重 + 满箱拒绝）。
//
// 收件箱实例由装配层注入（联邦域构造，直传包不构造）；未注入=未装配，
// 返回错误而非 panic（装配遗漏必须响亮，不能静默吞掉邀请）。
func (s *Service) AddInvite(code, fromName, note string) (Invite, error) {
	m := s.inviteMailbox()
	if m == nil {
		return Invite{}, errors.New("联邦收件箱未装配")
	}
	return m.AddInvite(code, fromName, note)
}

// ListInvites 未过期邀请（新到在前；委托联邦信封管理）。
//
// [退役 2026-10-22 / Wave B' 准备] HTTP 与 MCP 两面的联邦端点已**直接**取
// app/federation 实例（gen/handler/p2p 的 getFederationMailbox() 与
// bootstrap/adapters.go 的 mcpP2pAdapter.mailbox），本门面**已无调用方**。
// 保留它只为不在 v2.x 引入破坏性变更（删导出方法=升主版本）；Wave B' 删包时随之消失。
// 新增调用方请直接用 app/federation.InviteMailbox。
func (s *Service) ListInvites() []InviteSnapshot {
	m := s.inviteMailbox()
	if m == nil {
		return nil
	}
	return m.ListInvites()
}

// TakeInvite 认领（出箱；委托联邦信封管理）。
//
// 仍有一处调用方：端点 invites/accept 的第一步（第二步 spawn p2pc 收件在直传侧）。
// 这是 Wave B' 前**最后一处**「联邦 → 直传」的耦合点。
//
// 端点 invites/accept 的两步语义在此拆分：第一步"取信封"在联邦侧（本方法
// 委托），第二步"发起 p2pc 收件"留在直传侧（Service.Recv，需要 p2pc 运行时）。
// 生成物仍按原样分两步调用（先 TakeInvite 再 Recv），端点对外行为不变。
func (s *Service) TakeInvite(id string) (Invite, error) {
	m := s.inviteMailbox()
	if m == nil {
		return Invite{}, errors.New("联邦收件箱未装配")
	}
	return m.TakeInvite(id)
}

// PushInvite 把口令推送到目标实例的邀请收件端点（委托联邦信封管理）。
//
// [退役 2026-10-22 / Wave B' 准备] invite-push 端点已直接调 federation.PushInvite，
// 本包级函数**已无调用方**；保留只为避免 v2.x 破坏性变更。新增调用方请直接用
// app/federation.PushInvite。
func PushInvite(ctx context.Context, targetURL, code, fromName, note string) error {
	return federation.PushInvite(ctx, targetURL, code, fromName, note)
}
