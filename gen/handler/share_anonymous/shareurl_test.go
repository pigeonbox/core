package share_anonymous

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// 匿名取件码响应里 `url` 字段的形态契约（2026-10-23 真机取证）。
//
// 缺陷实录：GenerateCode 此前下发 "/share/select/?code={取件码}" ⇒ 第三方
// 直接访问该 URL 实测 **404**（/share/select 只认 8 位分享码，取件码查不到）
// ⇒ 那是一个**坏链**：既不是页面链接（取件码没有页面 URL——分享页路由
// /share/:code 不认它，取件码只能口头/短信报码），也不是能用的 API 路径。
//
// 判据：`url` 必须是**真实可用**的取件查询端点 GET /anonymous/search/{取件码}
// （真机守卫：scripts/smoke-full.sh S19 铸造后直接访问必须 200）。
func TestAnonymousQueryPath(t *testing.T) {
	got := anonymousQueryPath("WS9WHP")
	assert.Equal(t, "/anonymous/search/WS9WHP", got)

	// 不得再是 select 路径：取件码在 select 上查不到 ⇒ 404 坏链
	assert.NotContains(t, got, "/share/select")
	// 不得是页面形态：取件码没有页面链接（分享页不认它）
	assert.NotContains(t, got, "#")
}
