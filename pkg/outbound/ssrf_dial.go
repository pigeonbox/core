package outbound

import (
	"context"
	"fmt"
	"net"

	"github.com/pigeonbox/core/pkg/security"
)

// ssrfDialContext 拨号时刻 SSRF 复判（core#164 TOCTOU / DNS rebinding）：
// 保存期与解析期的校验各自解析过一次 DNS，实际连接再解析时答案可能已被换掉
// （解析期检的是公网 IP、拨号期解析到 169.254.169.254/内网）。
//
// 三步封死该面：
//  1. 拨号时**当场解析**（拿到本次连接真正要用的地址集）；
//  2. 逐 IP 过 security.BlockedIP——与保存期/重定向逐跳复验**同一判据**
//     （尊重 security.ssrf.allow_private_networks，不引入第二策略）；
//  3. 通过判定的 IP **直连**（net.JoinHostPort(已判定 IP, port)），不再把
//     hostname 交给底层二次解析——判定与连接用同一地址，rebinding 无空窗。
//
// 某 IP 被拒不等于整体失败：继续试地址集里下一个可用 IP（A 记录含多地址、
// 混排公私网时公网仍可达）；全部不可用才回错（拒绝类错误统一前缀"出站拨号
// 被拒绝"，错误链携带 security.ErrEndpointURL 可供结构性判定）。
//
// ⚠️ 边界：HTTP_PROXY 等代理环境变量由克隆的 DefaultTransport 继承——经代理
// 出站时 DialContext 拨的是**代理地址**，目标主机的拨号复判由代理侧完成
// （本层只复判代理 IP）；保存期校验与重定向逐跳复验不受影响。
func ssrfDialContext(base *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, ipa := range ips {
			if err := security.BlockedIP(ipa.IP); err != nil {
				lastErr = fmt.Errorf("出站拨号被拒绝: %w", err)
				continue
			}
			c, err := base.DialContext(ctx, network, net.JoinHostPort(ipa.IP.String(), port))
			if err == nil {
				return c, nil
			}
			lastErr = err
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("出站拨号被拒绝: 无可用安全地址 %q", addr)
		}
		return nil, lastErr
	}
}
