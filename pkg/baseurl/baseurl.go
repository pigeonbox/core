// Package baseurl 公开链接 base 解析（请求级公开地址推断的单源）。
//
// 语义（2026-10-10 自 app/share 域下沉，此前 presign/gen·handler/chunk·share
// 四处消费 share.ResolveBase 构成 app 跨域边与适配层对域词汇依赖）：
// 显式配置 > 请求来源（bootstrap 中间件注入 ctx）> 空串。
// 禁止回退 server.host——那是监听地址（0.0.0.0），拼进分享链接对外不可达
// （2026-10-07 iStoreOS 真机事故：分享成功弹窗给出 http://0.0.0.0:12345/#/s/x）。
// 双缺省时退相对路径（/#/share/CODE），浏览器侧可由 location.origin 补全。
package baseurl

import "context"

// publicBaseCtxKeyType 私有键类型（SA1029：ctx 键勿用内建 string，防跨包碰撞）。
type publicBaseCtxKeyType struct{}

// PublicBaseCtxKey 请求级公开 base 在 go ctx 中的键。bootstrap 在
// server.base_url 未配置时挂中间件，把每条请求的来源(scheme://host) 经
// context.WithValue 包进 go ctx 并 c.Next 透传——域服务读的是 handler 首参
// go ctx；c.Set 只落 RequestContext.Keys，service 层读不到（曾致推断死代码）。
var PublicBaseCtxKey publicBaseCtxKeyType

// Resolve 公开链接 base 解析：显式配置 > 请求来源（中间件注入）> 空串。
func Resolve(ctx context.Context, configured string) string {
	if configured != "" {
		return configured
	}
	if ctx != nil {
		if v, ok := ctx.Value(PublicBaseCtxKey).(string); ok && v != "" {
			return v
		}
	}
	return ""
}
