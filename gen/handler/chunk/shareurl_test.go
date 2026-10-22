package chunk

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// 分片通道对外链接的形态契约（2026-10-23 跨仓审计）。
//
// 缺陷实录：complete 与秒传两个出口都把下载令牌拼在 hash 形态页面链接
// 之后 ⇒ 产出 {base}/#/share/CODE?token=xx。令牌落在 fragment 内：
//   - 服务端收不到（'#' 之后不在请求行里）⇒ 直链鉴权形同虚设；
//   - 分享页前端也不读 route.query.token ⇒ 令牌纯属装饰；
//   - 却让集成方以为「这个链接带令牌，能直下」⇒ 误导。
//
// 判据（机械互锁）：页面链接形态固定为 {base}/#/share/{code}，
// 且**不含任何 query**；需要带令牌的直链走 /share/select 的 download_url。
func TestChunkSharePageURL_HashFormNoQuery(t *testing.T) {
	got := chunkSharePageURL(context.Background(), "CODE")

	// hash 路由形态（无配置 base 时退相对形态）
	assert.Equal(t, "/#/share/CODE", got)
	// 形态守卫：不得带 query（令牌不能落在 '#' 之后）
	assert.NotContains(t, got, "?")
	assert.NotContains(t, got, "token")
	// 形态守卫：不得退化成路径形态 {base}/share/CODE
	// （注意 "/#/share/CODE" 本身含 "/share/CODE" 子串，故按前缀判）
	assert.True(t, strings.HasPrefix(got, "/#/share/"), "页面链接必须是 hash 路由形态，实际=%q", got)
	assert.False(t, strings.HasPrefix(got, "/share/"), "不得是路径形态，实际=%q", got)
}
