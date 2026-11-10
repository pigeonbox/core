package outbound

import (
	"fmt"
	"net/http"

	"github.com/pigeonbox/core/pkg/security"
)

// maxRedirects 出站重定向上限（与 TargetNotify 同口径：5 跳）。
const maxRedirects = 5

// ssrfRedirectGuard 逐跳 SSRF 复验守卫（core#116）：每一跳重定向目标都必须过
// security.ValidateEndpointURL（与 app/federation 发送前的校验**同一函数**，收编
// 复用、不复制第二份校验），超 maxRedirects 跳中止。
//
// 为什么重定向目标不沿用「对端粒度 allow_private」放行：放行授权的是**登记的那
// 个端点 URL**，不是对端随手可指的任意目标——恶意对端 302 到
// 169.254.169.254/内网即 SSRF 绕过 + 邀请口令外带（core#116 原缺陷，此前
// http.DefaultClient 盲跟重定向）。故每一跳都按严格公网口径复验（内部尊重全局
// security.ssrf.allow_private_networks，与 TargetNotify 的重定向守卫同策略）。
func ssrfRedirectGuard(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	if err := security.ValidateEndpointURL(req.URL.String()); err != nil {
		return fmt.Errorf("出站重定向目标被拒绝: %w", err)
	}
	return nil
}
