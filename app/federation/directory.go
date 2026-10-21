// directory.go — 联邦节点名录透传（2026-10-14 邀请信箱配套；2026-10-22 由
// 直传包搬入）。
//
// 名录端点无鉴权（零敏感，公开信息），registry URL 由宿主从管理端
// SystemConfig 注入（已过 SSRF 校验）。直传未启用/二进制缺失不挡名录查询——
// 名录可见性只取决于 registry 可达。
//
// 归属：名录是对端站点列表，属联邦域，不属直传域——本文件不依赖 p2pc 运行时。
//
// 命名（2026-10-22 L3）：PeerDirectory = 联邦节点名录，**无条件**构造
// （可见性只取决于 registry 可达，与联邦运行时是否启用无关；见 invite.go
// 文件头的三类服务命名约定）。
package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pigeonbox/core/pkg/registry"
)

// DirectoryPeer 名录对端(与 p2pd GET /v1/directory 条目同形)。
type DirectoryPeer struct {
	NodeID      string `json:"node_id"`
	Name        string `json:"name"`
	URL         string `json:"url"`          // 对外基址(NAT 后可为空)
	BoxPub      string `json:"box_pub"`      // X25519 密封公钥 hex
	OnlineSince int64  `json:"online_since"` // 租约起始 unix 秒
}

// RegistryURLProvider 注册中心基址提供者；ok=false 表示该来源不提供值
// （段不存在 / 值为空）。
//
// 由宿主注入（配置读取面在本包之外——app 域不得互 import，配置读取在
// bootstrap 装配层），本包只认"基址字符串"。
type RegistryURLProvider func(ctx context.Context) (string, bool)

// ChainRegistryProvider 组合多个基址来源（按优先级）：前者提供值即用前者，
// 否则顺延到后者；全部不提供值则返回 ok=false（配置缺失）。
//
// 用途（2026-10-22 L4）：注册中心基址真相源改为**联邦段**
// （SystemConfig.p2p.federation.registry_url / conf.Federation.RegistryURL），
// 直传段（p2p.direct.registry_url）是搬迁前的历史来源，作存量兼容兜底——
// 只配了直传段的存量用户不会失去名录。
func ChainRegistryProvider(providers ...RegistryURLProvider) RegistryURLProvider {
	return func(ctx context.Context) (string, bool) {
		for _, p := range providers {
			if p == nil {
				continue
			}
			if u, ok := p(ctx); ok {
				return u, true
			}
		}
		return "", false
	}
}

// PeerDirectory 联邦节点名录客户端。
type PeerDirectory struct {
	registryURL RegistryURLProvider
}

// NewPeerDirectory 构造名录客户端（provider 由宿主注入；须非 nil）。
func NewPeerDirectory(provider RegistryURLProvider) *PeerDirectory {
	return &PeerDirectory{registryURL: provider}
}

// Peers 拉取在线名录(admin 目录对端选择器消费)。
// 直传未启用/二进制缺失不挡名录查询——名录可见性只取决于 registry 可达。
func (d *PeerDirectory) Peers(ctx context.Context) ([]DirectoryPeer, error) {
	if d.registryURL == nil {
		return nil, fmt.Errorf("联邦注册中心配置缺失")
	}
	raw, ok := d.registryURL(ctx)
	if !ok {
		return nil, fmt.Errorf("联邦注册中心配置缺失")
	}
	// 基址归一：多主备取首个 + 留空回填官方公共注册中心（pkg/registry 单源）。
	url := strings.TrimRight(registry.Effective(raw), "/") + "/v1/directory"
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("名录查询失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("名录查询失败: p2pd 返回 %d", resp.StatusCode)
	}
	var out struct {
		Nodes []DirectoryPeer `json:"nodes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("名录响应解析失败: %w", err)
	}
	return out.Nodes, nil
}
