package middleware

import (
	"net"
	"strings"
	"sync"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/oekit/pkg/convert"
)

// 可信代理配置（bootstrap 从 security.trusted_proxies 注入）。
//
// 安全语义：只有当"直连对端"（RemoteAddr）落在可信代理网段内时，才采信
// X-Forwarded-For / X-Real-IP；否则一律以直连地址为准。
// 这样反向代理部署（配置了 CIDR）下能取到真实客户端 IP，
// 而直连部署下伪造 XFF 无法绕过 IP 维度限流/锁定。
var (
	trustedMu    sync.RWMutex
	trustedCIDRs []string // CIDR 形态（裸 IP 已归一为 /32）
)

// SetTrustedProxies 配置可信代理网段（CIDR 或单 IP）。
// 空列表 = 不信任任何代理头（直连部署的安全默认）。
func SetTrustedProxies(cidrs []string) error {
	trustedMu.Lock()
	defer trustedMu.Unlock()
	parsed := make([]string, 0, len(cidrs))
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if !strings.Contains(c, "/") {
			c += "/32"
		}
		if _, _, err := net.ParseCIDR(c); err != nil {
			return err
		}
		parsed = append(parsed, c)
	}
	trustedCIDRs = parsed
	return nil
}

// trustedProxyCIDRs 返回可信代理 CIDR 清单（上游原语的入参形态）。
//
// 不复制：清单在 SetTrustedProxies 时整体替换、之后只读，直接返回底层切片即可
// （省掉每请求一次的拷贝）；上游只读该入参。
func trustedProxyCIDRs() []string {
	trustedMu.RLock()
	defer trustedMu.RUnlock()
	return trustedCIDRs
}

// ClientIP 解析客户端真实 IP（可信代理解析，XFF 从右向左）。
//
// 规则：
//  1. 直连地址不在可信代理网段 → 返回直连地址（忽略一切代理头）
//  2. 直连地址可信 → 从 X-Forwarded-For 最右端向左扫描，
//     跳过可信代理地址，返回第一个不可信地址（即真实客户端）
//  3. 无 XFF 时回退 X-Real-IP（仅当直连可信），再回退直连地址
func ClientIP(c *app.RequestContext) string {
	// RemoteAddr 返回的 net.Addr 接口值恒非 nil，直接取字符串
	return ResolveClientIP(remoteAddrString(c.RemoteAddr()),
		string(c.GetHeader("X-Forwarded-For")),
		string(c.GetHeader("X-Real-IP")))
}

// ResolveClientIP 可信代理解析核心（纯函数，便于单测覆盖伪造场景）。
// remote 为直连对端地址（host:port 或裸 IP）；xff/xri 为原始代理头。
//
// 采信判定与 XFF 可信链扫描收编上游 oekit/pkg/convert.ResolveClientIP（v0.25.2
// 随 #95 落地）——与 bianque 等消费方共用同一份实现，避免各仓各写一遍后口径漂移。
//
// 上游缺的两处边界在返回值上补回（上游缺口记录在 oekit/pkg#104）：
//  1. 代理头候选值不做合法性校验——任意字符串进限流/锁定键与审计日志，既是
//     「伪造 IP 永不命中计数 = 绕过」，也可注入任意内容污染日志；
//  2. 候选值带端口时未归一为裸 IP——同一客户端换个端口就能换出一个新桶。
func ResolveClientIP(remote, xff, xri string) string {
	if remote == "" {
		return "unknown"
	}
	ip := convert.ResolveClientIP(remote, xff, xri, trustedProxyCIDRs())
	// 归一为裸 IP（带端口的候选归一到同一分桶键）。
	if h, _, err := net.SplitHostPort(ip); err == nil {
		ip = h
	}
	// 2026-10-05 审计 P2：非法代理头值一律回退直连地址。
	if net.ParseIP(ip) == nil {
		return stripPort(remote)
	}
	return ip
}

// remoteAddrString net.Addr 安全取字符串（nil 防御）
func remoteAddrString(addr interface{ String() string }) string {
	if addr == nil {
		return ""
	}
	return addr.String()
}

// stripPort 去掉 IP:port 的端口部分（IPv6 [::1]:8080 同样处理）
func stripPort(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}
