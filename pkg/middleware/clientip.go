package middleware

import (
	"sync"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/oekit/pkg/convert"
)

// 可信代理配置（bootstrap 从 security.trusted_proxies 注入）。
//
// 安全语义：只有当"直连对端"（RemoteAddr）落在可信代理网段内时，才采信
// X-Forwarded-For；否则一律以直连地址为准。
// 这样反向代理部署（配置了 CIDR）下能取到真实客户端 IP，
// 而直连部署下伪造 XFF 无法绕过 IP 维度限流/锁定。
var (
	trustedMu      sync.RWMutex
	trustedProxies *convert.TrustedProxySet
)

// SetTrustedProxies 配置可信代理网段（CIDR 或单 IP）。
// 空列表 = 不信任任何代理头（直连部署的安全默认）。
//
// 条目解析收编上游 oekit/pkg/convert.TrustedProxySet（pkg v0.26.0）：
//   - 裸 IP 归一为单主机网段（IPv4 /32、IPv6 /128）；
//   - 非法条目**构造期报错**，不静默跳过——静默跳过把「配置写错」变成
//     「安全策略降级」且不可观测（此前裸 IP 被直接 continue 掉：配置不生效
//     且无任何告警，oekit/pkg#104）。
//
// 解析在构造期一次完成，请求期只做网段包含判定——此前每请求都要把整份清单
// 逐条 ParseCIDR 一遍（oekit/pkg#107）。
func SetTrustedProxies(cidrs []string) error {
	set, err := convert.NewTrustedProxySet(cidrs)
	if err != nil {
		return err
	}
	trustedMu.Lock()
	defer trustedMu.Unlock()
	trustedProxies = set
	return nil
}

// trustedProxySet 返回可信代理集合（上游原语的入参形态）。
//
// 不复制：集合在 SetTrustedProxies 时整体替换、之后只读，直接返回指针即可
// （上游只读该入参）。
func trustedProxySet() *convert.TrustedProxySet {
	trustedMu.RLock()
	defer trustedMu.RUnlock()
	return trustedProxies
}

// ClientIP 解析客户端真实 IP（可信代理解析，XFF 从右向左）。
//
// 规则：
//  1. 直连地址不在可信代理网段 → 返回直连地址（忽略一切代理头）
//  2. 直连地址可信 → 从 X-Forwarded-For 最右端向左扫描，
//     跳过可信代理地址，返回第一个不可信地址（即真实客户端）
//  3. XFF 缺失或整条链都没有合法候选 → 返回直连地址
//
// **不采信 X-Real-IP**：单跳头无法区分「代理写的」与「客户端自带的」——网关
// 默认不写该头但会原样透传客户端自带的，采信它等于让客户端任意改写自己的
// 客户端 IP（重置匿名配额 / 改写审计归因）。上游 oekit/pkg v0.26.0 起默认
// 同样不采信（oekit/pkg#106）。
func ClientIP(c *app.RequestContext) string {
	// RemoteAddr 返回的 net.Addr 接口值恒非 nil，直接取字符串
	return ResolveClientIP(remoteAddrString(c.RemoteAddr()),
		string(c.GetHeader("X-Forwarded-For")))
}

// ResolveClientIP 可信代理解析核心（纯函数，便于单测覆盖伪造场景）。
// remote 为直连对端地址（host:port 或裸 IP）；xff 为原始代理头。
//
// 采信判定与 XFF 可信链扫描收编上游 oekit/pkg/convert.ResolveClientIP
// （v0.25.2 随 #95 落地，v0.26.0 随 #104/#106/#107 收紧）——与 bianque 等
// 消费方共用同一份实现，避免各仓各写一遍后口径漂移。
//
// 本仓此前自持的两处加固已由上游补齐，本地不再重复实现：
//  1. 代理头候选值合法性校验——任意字符串进限流/锁定键与审计日志，既是
//     「伪造 IP 永不命中计数 = 绕过」，也可注入任意内容污染日志；
//  2. 候选值带端口时归一为裸 IP——同一客户端换个端口就能换出一个新桶。
//
// 空对端地址返回 "unknown"（本仓约定：调用方按字符串拼进限流/锁定键，
// 空串会让"未知来源"与"真实空值"混在一起）。
func ResolveClientIP(remote, xff string) string {
	if remote == "" {
		return "unknown"
	}
	return convert.ResolveClientIPWithSet(remote, xff, trustedProxySet())
}

// remoteAddrString net.Addr 安全取字符串（nil 防御）
func remoteAddrString(addr interface{ String() string }) string {
	if addr == nil {
		return ""
	}
	return addr.String()
}
