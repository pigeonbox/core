package security

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/oekit/concurrency/singleflight"
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
// 未命中的并发解析经 singleflight 合并：同 host 共享一次 LookupIP，
// 不同 host 并行互不阻塞（历史实现持全局锁横跨 DNS 调用，会把全部
// 校验串行化在一次慢解析之后）。
var (
	dnsCache   = map[string]dnsCacheEntry{}
	dnsCacheMu sync.Mutex
	dnsFlight  = singleflight.New[[]net.IP]()
)

type dnsCacheEntry struct {
	ips  []net.IP
	at   time.Time
	priv bool
}

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
		if endpointGuard.CheckIP(ip) != nil {
			return fmt.Errorf("%w: 端点解析到私网/保留地址 %s；如为局域网存储请在配置中开启 security.ssrf.allow_private_networks", ErrEndpointURL, ip)
		}
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
		if endpointGuard.CheckIP(ip) != nil {
			return fmt.Errorf("%w: 端点解析到私网/保留地址 %s；如为局域网存储请在配置中开启 security.ssrf.allow_private_networks", ErrEndpointURL, ip)
		}
	}
	return nil
}

// resolveHost 带缓存的 DNS 解析（结果缓存 5 分钟；解析失败不缓存，下次重试）。
func resolveHost(host string) []net.IP {
	if ips, ok := dnsCacheLookup(host); ok {
		return ips
	}
	ips, _ := dnsFlight.Do(context.Background(), host, func(ctx context.Context) ([]net.IP, error) {
		// 双检:等待合并期间可能已被同批调用写入缓存
		if ips, ok := dnsCacheLookup(host); ok {
			return ips, nil
		}
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		ips := make([]net.IP, len(addrs))
		for i, a := range addrs {
			ips[i] = a.IP
		}
		dnsCacheMu.Lock()
		dnsCache[host] = dnsCacheEntry{ips: ips, at: time.Now()}
		dnsCacheMu.Unlock()
		return ips, nil
	})
	return ips
}

// dnsCacheLookup 读取未过期的缓存解析结果（ok=false = 未命中/已过期）。
func dnsCacheLookup(host string) ([]net.IP, bool) {
	dnsCacheMu.Lock()
	defer dnsCacheMu.Unlock()
	if e, ok := dnsCache[host]; ok && time.Since(e.at) < 5*time.Minute {
		return e.ips, true
	}
	return nil, false
}
