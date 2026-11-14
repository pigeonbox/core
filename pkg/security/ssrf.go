package security

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/oekit/pkg/cache"
	"github.com/oekit/pkg/urlguard"
	"github.com/pigeonbox/core/conf"
)

// ErrEndpointURL 存储端点 URL 不合法
var ErrEndpointURL = fmt.Errorf("存储端点 URL 不合法")

// endpointGuard 出站地址的网段判定器（oekit/pkg/urlguard）。
//
// 阻断范围比本仓历史口径更全：除 loopback/RFC1918/链路本地/未指定外，
// 还挡组播/保留段（224/4、240/4）、0.0.0.0/8、IPv6 组播与 ULA。
//
// **CGNAT 100.64.0.0/10 显式豁免**：Tailscale 等 overlay 网络的节点地址
// 落在该段，历史口径不拦它——端点就绪性由用户自己负责，显式豁免以保持
// 既有行为（豁免写在代码里而不是靠运气）。
var endpointGuard = mustEndpointGuard()

func mustEndpointGuard() *urlguard.Guard {
	g, err := urlguard.New(urlguard.WithAllowCIDRs("100.64.0.0/10"))
	if err != nil {
		// 固定字面量，构造失败只可能是代码写错——带病运行没有意义。
		panic(fmt.Sprintf("security: urlguard: %v", err))
	}
	return g
}

// dnsCache 端点解析结果短缓存，避免每次校验都打 DNS（5 分钟 TTL）。
// oekit/pkg/cache.LocalCache（TTL 过期回收 + 后台清扫）+ SingleFlightCache
// 装饰器（并发未命中合并成一次 LookupIP，不同 host 并行互不阻塞——历史实现
// 持全局锁横跨 DNS 调用，会把全部校验串行化在一次慢解析之后）。
//
// 语义与历史手搓 map 版对齐：解析失败不缓存（Source 回错即透传，下次重试）；
// 「解析成功但无地址」缓存为真值（避免每请求重解析）。顺带修掉旧版两个小
// 毛病：过期条目永不清理（多 host 场景下 map 无界增长）、entry.priv 死字段。
var dnsCache = cache.NewSingleFlightCache(cache.NewLocalCache(cache.LocalConfig[[]net.IP]{
	TTL:     5 * time.Minute,
	NullTTL: -1, // 不写 null marker：Source 只在解析成功时返回非 nil
	Source:  lookupHostIP,
}))

// ValidateEndpointURL 校验 s3/webdav 存储端点 URL：
//   - 仅允许 http/https scheme
//   - 必须携带 host
//   - DNS 解析后逐 IP 复判私网策略（防重绑定攻击：解析结果非配置期望网段时拒绝）
//
// allowPrivate 未配置（默认 false）时拒绝环回/私网/链路本地地址。
// 局域网 MinIO/WebDAV（飞牛 NAS）部署需显式 security.ssrf.allow_private_networks=true。
func ValidateEndpointURL(rawURL string) error {
	return validateEndpointURL(rawURL, false)
}

// ValidateEndpointURLAllowPrivate 校验 URL 形态（scheme/host）但**放行私网**。
//
// 用途（2026-11-01 联邦前台化）：跨站投递的「可投递对端」按**对端粒度**开
// allow_private（自托管场景对方柜常在局域网）⇒ 不能要求管理员去开全局
// security.ssrf.allow_private_networks（一开就是所有出站点位都放行私网，面太大）。
//
// ⚠️ 本函数**只**放行私网判定，不放松 scheme/host 校验（javascript:/data:
// 这类可执行 scheme 与空 host 照样拒）。
func ValidateEndpointURLAllowPrivate(rawURL string) error {
	return validateEndpointURL(rawURL, true)
}

// validateEndpointURL 端点 URL 校验本体。
//
// allowPrivatePeer=true = 放行私网判定（**按对端粒度**的显式放行）；
// 全局开关 security.ssrf.allow_private_networks 仍然独立生效（它一开就全放行）。
func validateEndpointURL(rawURL string, allowPrivatePeer bool) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrEndpointURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: 仅支持 http/https 协议，当前为 %q", ErrEndpointURL, u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: 缺少 host", ErrEndpointURL)
	}

	allowPrivate := allowPrivatePeer
	if cfg := conf.GetGlobalConfig(); cfg != nil && cfg.Security.SSRF.AllowPrivateNetworks {
		allowPrivate = true
	}
	if allowPrivate {
		return nil
	}

	// IP 直写或域名解析，逐 IP 校验
	ips := resolveHost(host)
	if len(ips) == 0 {
		// 解析失败不在此处定性（连接阶段会自然失败），仅放行 scheme/host 校验
		return nil
	}
	for _, ip := range ips {
		if err := BlockedIP(ip); err != nil {
			return err
		}
	}
	return nil
}

// BlockedIP 单 IP 私网/保留地址判定（私网/回环/链路本地/组播/保留段等；
// 尊重全局 security.ssrf.allow_private_networks——开启即全放行）。
//
// validateEndpointURL / ValidateEndpointHost 与出站拨号时刻复核（core#164，
// pkg/outbound 的 ssrfDialContext）**共用同一判据**——不写第二份（抽出即
// 复用：拨号层拿到的是已解析 IP，没有 URL 可喂，故按单 IP 粒度导出）。
func BlockedIP(ip net.IP) error {
	if cfg := conf.GetGlobalConfig(); cfg != nil && cfg.Security.SSRF.AllowPrivateNetworks {
		return nil
	}
	if endpointGuard.CheckIP(ip) != nil {
		return fmt.Errorf("%w: 端点解析到私网/保留地址 %s；如为局域网存储请在配置中开启 security.ssrf.allow_private_networks", ErrEndpointURL, ip)
	}
	return nil
}

// ValidateEndpointHost 校验裸主机端点（FTP/SFTP 的 host 字段：host[:port]，
// 无 scheme 不适用 URL 解析）。策略与 ValidateEndpointURL 一致：IP 字面量直接
// 判私网；域名解析后逐 IP 复判；allow_private_networks 开启时全放行。
func ValidateEndpointHost(hostPort string) error {
	host := hostPort
	if h, _, err := net.SplitHostPort(strings.TrimSpace(hostPort)); err == nil {
		host = h
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return fmt.Errorf("%w: 缺少 host", ErrEndpointURL)
	}
	allowPrivate := false
	if cfg := conf.GetGlobalConfig(); cfg != nil {
		allowPrivate = cfg.Security.SSRF.AllowPrivateNetworks
	}
	if allowPrivate {
		return nil
	}
	ips := resolveHost(host)
	for _, ip := range ips {
		if err := BlockedIP(ip); err != nil {
			return err
		}
	}
	return nil
}

// lookupHostIP DNS 解析回源函数（LocalCache.Source）。
func lookupHostIP(ctx context.Context, host string) (*[]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, len(addrs))
	for i, a := range addrs {
		ips[i] = a.IP
	}
	return &ips, nil
}

// resolveHost 带缓存的 DNS 解析（结果缓存 5 分钟；解析失败不缓存，下次重试）。
func resolveHost(host string) []net.IP {
	ips, err := dnsCache.GetOrLoad(context.Background(), host)
	if err != nil || ips == nil {
		return nil
	}
	return *ips
}
