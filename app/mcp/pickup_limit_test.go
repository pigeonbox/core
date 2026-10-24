// MCP 面的「取件次数上限」回归（2026-10-24）
//
// 缺陷：model.FileCode.IsExpired() 拆分前**含次数语义**（ExpiredCount==0 ⇒
// 过期）⇒ MCP 的 download_share_file / get_share_content 在 count 样式分享
// 额度用完后都会拒（IsExpired 命中）。拆分后 IsExpired() 只判时间 ⇒ 这两个
// 工具**漏掉了次数闸** ⇒
//   - download_share_file 仍把文件内容 base64 回传；
//   - get_share_content 仍把**文本正文**回传；
//
// ⇒ MCP 建分享时选的 count 样式（按取件次数）承诺形同虚设：owner 说「最多取
// N 次」，额度用完后管理面照样能读走全部内容。
//
// 断言必须能变红（负对照）：把 app/mcp/tools.go 里两处 IsCountExhausted 闸门
// 删掉，本用例即失败。
package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/model"
)

// TestToolsCall_CountExhaustedShareDenied 额度用完后，MCP 两个读内容工具都要拒。
func TestToolsCall_CountExhaustedShareDenied(t *testing.T) {
	s, g := newMCPTestServiceWithDB(t)

	// 文本分享：count 样式（按取件次数，1 次）⇒ 建出来就是「最多取 1 次」
	_, out := rpc(t, s, "tools/call", map[string]any{
		"name": "share_text",
		"arguments": map[string]any{
			"text":         "机密正文-不应在额度用完后被读走",
			"expire_style": "count",
			"expire_value": 1,
		},
	})
	require.False(t, toolIsErr(out), "share_text 应成功: %s", toolErrText(t, out))
	code, _ := toolOut(t, out)["code"].(string)
	require.Len(t, code, 8)

	// 额度未用完 ⇒ 两个工具都读得到（正向对照：闸门不是「永远拒」）
	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "get_share_content", "arguments": map[string]any{"code": code},
	})
	require.False(t, toolIsErr(out), "额度未用完时 get_share_content 应成功: %s", toolErrText(t, out))
	assert.Equal(t, "机密正文-不应在额度用完后被读走", toolOut(t, out)["text"])

	// 把剩余次数扣到 0 ⇒ 分享耗尽
	require.NoError(t, g.Model(&model.FileCode{}).
		Where("code = ?", code).
		Update("expired_count", 0).Error)

	// 负向：正文不得再被读走
	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "get_share_content", "arguments": map[string]any{"code": code},
	})
	assert.True(t, toolIsErr(out), "额度用完后 get_share_content 必须拒绝")
	assert.Contains(t, toolErrText(t, out), "取件次数已用完")

	// 负向：download_share_file 对文本分享本来就走「用 get_share_content」引导，
	// 但**不得**因为额度耗尽而给出别的误导性结论（这里只断言它没返回内容）
	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "download_share_file", "arguments": map[string]any{"code": code},
	})
	assert.True(t, toolIsErr(out), "额度用完后 download_share_file 必须拒绝")
	assert.Contains(t, toolErrText(t, out), "取件次数已用完")
}

// TestToolsCall_CountExhaustedFileShareDenied 文件分享同样守闸（回传文件内容）。
func TestToolsCall_CountExhaustedFileShareDenied(t *testing.T) {
	s, g := newMCPTestServiceWithDB(t)

	b64 := "c2VjcmV0LWJ5dGVz"
	_, out := rpc(t, s, "tools/call", map[string]any{
		"name": "share_file",
		"arguments": map[string]any{
			"file_name":      "secret.txt",
			"content_base64": b64,
			"expire_style":   "count",
			"expire_value":   1,
		},
	})
	require.False(t, toolIsErr(out), "share_file 应成功: %s", toolErrText(t, out))
	code, _ := toolOut(t, out)["code"].(string)
	require.Len(t, code, 8)

	// 正向对照：额度未用完时可下载
	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "download_share_file", "arguments": map[string]any{"code": code},
	})
	require.False(t, toolIsErr(out), "额度未用完时 download_share_file 应成功: %s", toolErrText(t, out))
	assert.NotEmpty(t, toolOut(t, out)["content_base64"])

	// 额度用完 ⇒ 必须拒绝（否则文件内容仍被 base64 回传）
	require.NoError(t, g.Model(&model.FileCode{}).
		Where("code = ?", code).
		Update("expired_count", 0).Error)

	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "download_share_file", "arguments": map[string]any{"code": code},
	})
	assert.True(t, toolIsErr(out), "额度用完后 download_share_file 必须拒绝")
	assert.Contains(t, toolErrText(t, out), "取件次数已用完")

	// 不得泄露内容
	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "download_share_file", "arguments": map[string]any{"code": code},
	})
	assert.NotContains(t, toolErrText(t, out), b64, "拒绝时不得回传内容")
}
