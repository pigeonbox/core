// P2P 联邦解析代理（GET /api/v1/federation/resolve，手写路由）。
// 前端在录码本站未命中时经此查询联邦，命中则直跳源节点取件页预填口令；
// 本端点只做"发现"，不代理文件流量（下载始终直连源节点、源节点本地校验）。
// 未启用联邦时统一 available:false，不区分"未启用/不存在"（降探测面）；
// registry 侧对 resolve 限流最严，本端点不再叠加限流（M4 视压测调参）。
package handler

import (
	"context"
	"strings"
	"sync"

	"github.com/cloudwego/hertz/pkg/app"

	"github.com/pigeonbox/core/app/federation"
	"github.com/pigeonbox/core/pkg/resp"
)

// federationSvc 读写走 federationMu——P1 后台热切换=运行期写，与请求路径并发。
var (
	federationSvc *federation.Service
	federationMu  sync.RWMutex
)

// SetFederationService 注入联邦服务（bootstrap 调用；nil = 未启用）
func SetFederationService(s *federation.Service) {
	federationMu.Lock()
	defer federationMu.Unlock()
	federationSvc = s
}

// currentFederationService 读当前联邦服务（并发安全快照）
func currentFederationService() *federation.Service {
	federationMu.RLock()
	defer federationMu.RUnlock()
	return federationSvc
}

// FederationResolve 口令联邦解析。
func FederationResolve(ctx context.Context, c *app.RequestContext) {
	svc := currentFederationService()
	if svc == nil {
		resp.Success(c, map[string]any{"available": false})
		return
	}
	code := string(c.Query("code"))
	if code == "" {
		resp.Success(c, map[string]any{"available": false})
		return
	}
	info, err := svc.Resolve(code)
	if err != nil || info == nil {
		resp.Success(c, map[string]any{"available": false})
		return
	}
	// 对端 URL scheme 白名单（2026-10-05 审计 P3）：registry 返回的 URL 交由
	// 前端跳转，仅放行 http(s)，其余（含畸形串）按未命中处理，防恶意节点
	// 借本端点向访问者浏览器注入非 http 跳转目标。
	u := strings.TrimSpace(info.URL)
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		resp.Success(c, map[string]any{"available": false})
		return
	}
	resp.Success(c, map[string]any{
		"available":  true,
		"url":        u,
		"node_id":    info.NodeID,
		"name":       info.Name,
		"expires_at": info.ExpiresAt.Unix(),
	})
}
