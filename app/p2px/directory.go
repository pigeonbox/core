// directory.go — p2pd 在线名录透传(2026-10-14 邀请信箱配套)。
// p2px 侧轻量 HTTP 客户端:名录端点无鉴权(零敏感,公开信息),
// registry URL 来自 admin SystemConfig(已过 SSRF 校验)。
package p2px

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// DirectoryPeer 名录对端(与 p2pd GET /v1/directory 条目同形)。
type DirectoryPeer struct {
	NodeID      string `json:"node_id"`
	Name        string `json:"name"`
	URL         string `json:"url"`          // 对外基址(NAT 后可为空)
	BoxPub      string `json:"box_pub"`      // X25519 密封公钥 hex
	OnlineSince int64  `json:"online_since"` // 租约起始 unix 秒
}

// DirectoryPeers 拉取在线名录(admin 目录对端选择器消费)。
// 直传未启用/二进制缺失不挡名录查询——名录可见性只取决于 registry 可达。
func (s *Service) DirectoryPeers(ctx context.Context) ([]DirectoryPeer, error) {
	cfg, ok := s.deps.GetDirect(ctx)
	if !ok {
		return nil, fmt.Errorf("直传配置缺失")
	}
	url := strings.TrimRight(effectiveRegistry(cfg.RegistryURL), "/") + "/v1/directory"
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
