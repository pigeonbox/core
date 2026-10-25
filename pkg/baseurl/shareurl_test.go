package baseurl

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// 分享链接形态契约（2026-10-22 真机定位的「死链」修复）。
//
// 这组断言是**格式契约**，不是实现细节：任何把页面链接退回路径形态
// {base}/share/{code} 的改动都会让 MCP/脚本/IM 机器人拿到的链接打不开
// （docker 端 404、原生端白屏），任何把下载令牌挪进 '#' 之后的改动都会
// 让服务端收不到令牌 ⇒ 直链鉴权失效。

func TestSharePageURL(t *testing.T) {
	// 绝对形态：hash 路由（页面地址的路由段在 '#' 之后）
	assert.Equal(t, "http://10.10.10.3:56789/#/share/AbCdEfGh", SharePageURL("http://10.10.10.3:56789", "AbCdEfGh"))
	// base 为空 → 相对形态（浏览器按 location.origin 补全）
	assert.Equal(t, "/#/share/AbCdEfGh", SharePageURL("", "AbCdEfGh"))
	// base 尾斜杠不产生双斜杠
	assert.Equal(t, "https://s.example.com/#/share/AbCdEfGh", SharePageURL("https://s.example.com/", "AbCdEfGh"))

	// 形态守卫：**不得**出现路径形态的 /share/CODE（'#' 之前）
	got := SharePageURL("http://h:1", "CODE")
	assert.Contains(t, got, "/#/share/CODE")
	assert.NotContains(t, got, "http://h:1/share/CODE")
}

func TestShareDownloadURL(t *testing.T) {
	// 直链：令牌走 query（服务端要校验 ⇒ 必须在 '#' 之前可见）
	assert.Equal(t, "http://h:1/share/download?code=CODE&token=TK",
		ShareDownloadURL("http://h:1", "CODE", "TK"))
	// 无令牌（下载令牌功能关闭）
	assert.Equal(t, "http://h:1/share/download?code=CODE",
		ShareDownloadURL("http://h:1", "CODE", ""))
	// 相对形态
	assert.Equal(t, "/share/download?code=CODE&token=TK",
		ShareDownloadURL("", "CODE", "TK"))

	// 形态守卫：直链**不得**带 '#'（令牌落在 '#' 之后服务端收不到）
	assert.NotContains(t, ShareDownloadURL("http://h:1", "CODE", "TK"), "#")
}

func TestSplitSharePageURL(t *testing.T) {
	// 新形态（hash 路由）
	base := SplitSharePageURL("http://h:1/#/share/CODE", "CODE")
	assert.Equal(t, "http://h:1", base)

	// 旧形态 {base}/share/{code} **不再识别**（2026-10-25 删除旧版本兼容）：
	// 服务端自产的分享链接恒为 hash 形态 ⇒ 旧分支不可达 ⇒ 旧链接不再被派生直链。
	base2 := SplitSharePageURL("http://h:1/share/CODE", "CODE")
	assert.Equal(t, "http://h:1/share/CODE", base2, "旧形态应原样返回（不再派生直链）")

	// 相对形态
	base3 := SplitSharePageURL("/#/share/CODE", "CODE")
	assert.Equal(t, "", base3)
	base4 := SplitSharePageURL("/share/CODE", "CODE")
	assert.Equal(t, "/share/CODE", base4, "旧形态应原样返回")

	// 不匹配时原样返回（不吞信息）
	base5 := SplitSharePageURL("http://h:1/other", "CODE")
	assert.Equal(t, "http://h:1/other", base5)
}
