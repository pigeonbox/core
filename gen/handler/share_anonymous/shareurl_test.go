package share_anonymous

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// 匿名取件码响应里 `url` 字段的形态契约（2026-10-23 三轮演进后的定论）。
//
// ① 最初下发 "/share/select/?code={取件码}" ⇒ 实测直接访问 **404**
//    （/share/select 只认 8 位分享码，取件码查不到）⇒ 坏链。
// ② 改下发 "/anonymous/search/{取件码}" ⇒ 能访问，但那是 **API 路径**
//    （返回 JSON 元数据，不是给人点的页面），且与其它端点的 `url` 口径不一致。
// ③ 现在分享域已认 6 位取件码 ⇒ 取件码**有了**页面链接 ⇒ 与 /share/file/、
//    /share/text/ 等端点统一给**页面链接**（hash 路由形态）。
//
// 真机守卫：scripts/smoke-full.sh S16d/S16e。
func TestAnonymousPageURL(t *testing.T) {
	got := anonymousPageURL(context.Background(), "WS9WHP")

	// hash 路由形态（无 base_url 配置时退相对形态）
	assert.Equal(t, "/#/share/WS9WHP", got)
	assert.Contains(t, got, "/#/share/")

	// 不得再是 API 路径（坏链 / 非页面形态）
	assert.NotContains(t, got, "/share/select")
	assert.NotContains(t, got, "/anonymous/search")
	assert.NotContains(t, got, "?")
}
