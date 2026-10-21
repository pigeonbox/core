// baseurl_provider_test.go —— 对外 base 地址的动态来源（2026-10-22 站点公网地址）。
//
// 背景：分享链接与直传回跳地址此前用**构造期**的 baseURL（env/yaml 的
// server.base_url），而 server.base_url 只能在 yaml/env 里改 —— NAS/容器
// 打包面的用户不碰 .env，于是分享链接带内网 IP，外部用户打不开。
//
// 本组用例钉住 resolveBaseURL 的优先级：
//
//	provider 非空且返回值非空 → 用它（管理后台填的站点公网地址，改完立即生效）
//	provider 为 nil / 返回空 / 返回纯空白 → 回退构造期 baseURL
//	两者都空 → 交给 baseurl.Resolve 按请求来源动态推断（publicBaseMiddleware）
package share

import (
	"context"
	"testing"
)

func TestResolveBaseURL(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name     string
		baseURL  string
		provider func(context.Context) string
		want     string
	}{
		{
			name:    "无 provider 时用构造期值",
			baseURL: "https://from-yaml.example.com",
			want:    "https://from-yaml.example.com",
		},
		{
			name:    "无 provider 且构造期也空 → 空（按请求来源推断）",
			baseURL: "",
			want:    "",
		},
		{
			name:     "provider 优先于构造期值",
			baseURL:  "https://from-yaml.example.com",
			provider: func(context.Context) string { return "https://from-ui.example.com" },
			want:     "https://from-ui.example.com",
		},
		{
			name:     "provider 返回空 → 回退构造期值",
			baseURL:  "https://from-yaml.example.com",
			provider: func(context.Context) string { return "" },
			want:     "https://from-yaml.example.com",
		},
		{
			name:     "provider 返回纯空白 → 回退构造期值（视为未配置）",
			baseURL:  "https://from-yaml.example.com",
			provider: func(context.Context) string { return "   \t\n " },
			want:     "https://from-yaml.example.com",
		},
		{
			name:     "provider 返回值原样透出（不 trim 内部空白）",
			baseURL:  "",
			provider: func(context.Context) string { return "https://box.example.com/path" },
			want:     "https://box.example.com/path",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Service{baseURL: c.baseURL}
			if c.provider != nil {
				s.SetBaseURLProvider(c.provider)
			}
			if got := s.resolveBaseURL(ctx); got != c.want {
				t.Fatalf("resolveBaseURL = %q, 期望 %q", got, c.want)
			}
		})
	}
}

// TestResolveBaseURLProviderReadsCtx provider 拿到请求 ctx（多副本广播刷新
// 后的当前配置靠 ctx 取，不能闭包捕获一次性快照）。
func TestResolveBaseURLProviderReadsCtx(t *testing.T) {
	type ctxKey struct{}
	s := &Service{baseURL: "https://fallback.example.com"}
	s.SetBaseURLProvider(func(ctx context.Context) string {
		if v, ok := ctx.Value(ctxKey{}).(string); ok {
			return v
		}
		return ""
	})
	ctx := context.WithValue(context.Background(), ctxKey{}, "https://per-request.example.com")
	if got := s.resolveBaseURL(ctx); got != "https://per-request.example.com" {
		t.Fatalf("provider 未读到 ctx 值，实际 %q", got)
	}
}
