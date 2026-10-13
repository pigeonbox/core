package baseurl

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// ResolveBase 解析顺序回归:显式配置 > 请求来源(中间件注入 ctx) > 空串退相对路径。
// 禁止回退 server.host——监听地址(0.0.0.0)拼进分享链接对外不可达
// (2026-10-07 iStoreOS 真机事故:分享成功弹窗 http://0.0.0.0:12345 链接)。
func TestResolve(t *testing.T) {
	// 显式配置最优先
	assert.Equal(t, "https://s.example.com", Resolve(context.Background(), "https://s.example.com"))

	// base_url 未配置时取请求来源(bootstrap 中间件写入 go ctx)。
	// 键为类型化私有键(SA1029):消费路径是 go ctx(c.Next 透传),
	// 与 hertz RequestContext.Keys(string 键)无关——那是 c.Set 时代的历史约束。
	ctx := context.WithValue(context.Background(), PublicBaseCtxKey, "http://10.10.30.250:12345")
	assert.Equal(t, "http://10.10.30.250:12345", Resolve(ctx, ""))

	// 双缺省退空串(FullShareURL 退化为相对路径,前端可由 origin 补全)
	assert.Equal(t, "", Resolve(context.Background(), ""))
}
