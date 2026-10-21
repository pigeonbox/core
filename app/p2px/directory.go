// directory.go — p2pd 在线名录透传在直传包侧的兼容门面（2026-10-22，Wave B
// B0 波）。
//
// 实现已搬至 app/federation（名录属联邦域，不属直传域）：本文件只保留同名
// 委托面，使生成物（gen/handler/p2p/*）与 MCP 适配器零改动。
package p2px

import (
	"context"
	"errors"

	"github.com/pigeonbox/core/app/federation"
)

// DirectoryPeer 名录对端(类型别名 = federation.DirectoryPeer：结构同形)。
type DirectoryPeer = federation.DirectoryPeer

// DirectoryPeers 拉取在线名录(admin 目录对端选择器消费；委托联邦名录)。
//
// [退役 2026-10-22 / Wave B' 准备] directory 端点与 MCP p2p_peers 已**直接**取
// app/federation.PeerDirectory，本门面**已无调用方**；保留只为避免 v2.x 破坏性变更。
// 新增调用方请直接用 app/federation.PeerDirectory。
//
// 名录实例由装配层注入（联邦域构造，直传包不构造）；未注入=未装配，
// 返回错误而非 panic。
func (s *Service) DirectoryPeers(ctx context.Context) ([]DirectoryPeer, error) {
	d := s.peerDirectory()
	if d == nil {
		return nil, errors.New("联邦名录未装配")
	}
	return d.Peers(ctx)
}
