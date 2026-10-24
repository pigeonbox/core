// 文本分享的计次语义回归（2026-10-24）
//
// 缺陷（上一轮改动引入的**功能性回归**）：文本分享改成「交付即扣次」后，
// 计次键取的是**本次 select 新签发的令牌指纹** ⇒ 每刷新一次页面就是一次新
// 令牌 ⇒ **刷新即扣次** ⇒ 收件人打开页面刷新两次就把 owner 的额度吃光，
// 第三次 select 直接 403「超过取件次数」——**正文都看不了**。
//
// 修复后的语义：文本交付的计次键 = (分享码, 取件人 IP) ⇒
//   - 同一个人重复打开/刷新 ⇒ 同一个键 ⇒ 只扣 1 次（能一直看）；
//   - 不同的人（不同 IP）⇒ 各自扣 1 次 ⇒ owner 的「N 次」= N 个不同取件者。
//
// 文件分享不受影响：交付动作 = 下载 ⇒ 一次下载会话（令牌）= 一次取件。
package share

import (
	"net/url"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostHeader 测试请求的 Host 头（与既有下载测试同口径）。
func hostHeader() ut.Header { return ut.Header{Key: "Host", Value: "test.local"} }

// 文本分享：同一取件人连续两次 select ⇒ 只扣 1 次，第二次仍能拿到正文
func TestSelect_TextShare_SameViewerChargesOnce(t *testing.T) {
	g := newChargeTestEnv(t, false)
	insertFileCode(t, g, &model.FileCode{
		Code: "TXT001", Text: "hello-text", FilePath: "", ExpiredCount: 2,
	})

	// 第一次：交付正文 + 扣 1 次
	status, body := performGet(t, "/share/select/", "code=TXT001", hostHeader())
	assert.Equal(t, 200, status, "首次取件应成功：%s", body)
	assert.Contains(t, body, "hello-text", "首次应交付正文")

	// 第二次（同一取件人刷新页面）⇒ 仍 200、仍交付正文、**不重复扣次**
	status, body = performGet(t, "/share/select/", "code=TXT001", hostHeader())
	assert.Equal(t, 200, status, "同一取件人刷新页面不应被拒：%s", body)
	assert.Contains(t, body, "hello-text", "刷新后仍应交付正文")

	remaining, used := usedCount(t, g, "TXT001")
	assert.Equal(t, 1, remaining, "同一取件人只扣 1 次（此前每次刷新都扣）")
	assert.Equal(t, 1, used)
}

// 文本分享：count=1 时，末次取件后同一取件人刷新仍能看到正文
// （回归前的行为：第二次 select 就 403，正文都拿不到）
func TestSelect_TextShare_SecondSelectNotBlocked(t *testing.T) {
	g := newChargeTestEnv(t, false)
	insertFileCode(t, g, &model.FileCode{
		Code: "TXT003", Text: "the-body", FilePath: "", ExpiredCount: 1,
	})

	// 第一次：成功，额度正好用完
	status, body := performGet(t, "/share/select/", "code=TXT003", hostHeader())
	require.Equal(t, 200, status, "末次取件应成功：%s", body)

	// 第二次（同一取件人刷新）⇒ 仍能看（回归前这里是 403）
	status, body = performGet(t, "/share/select/", "code=TXT003", hostHeader())
	assert.Equal(t, 200, status, "同一取件人刷新不应被 403：%s", body)
	assert.Contains(t, body, "the-body")
}

// 文件分享（令牌开启，生产默认）：每次**下载会话**各扣 1 次
//
// 文件的交付动作是「下载」⇒ 判重粒度是**下载会话**（令牌）：
// 同一会话内的多文件逐个下载/断点续传 = 一次取件（只扣 1 次）；
// 重新取件（重新 select ⇒ 新令牌）再下载 = 一次新的取件（再扣 1 次）。
func TestDownload_FileShare_StillChargesPerSession(t *testing.T) {
	g := newChargeTestEnv(t, true) // 令牌开启
	insertFileCode(t, g, &model.FileCode{
		Code: "FILE01", Text: "a.bin", FilePath: "uploads", UUIDFileName: "a.bin",
		Size: 10, ExpiredCount: 5,
	})
	// select 取令牌（文件分享不交付内容 ⇒ 不扣次）
	resp := selectShare(t, "FILE01")
	require.NotEmpty(t, resp.Data.Token)
	tok := "&token=" + url.QueryEscape(resp.Data.Token)

	status, body := performDownloadPath(t, "code=FILE01"+tok)
	require.Equal(t, 200, status, "下载应成功：%s", body)

	// 换一个会话（新令牌）再下载 ⇒ 扣第 2 次（文件交付按会话计）
	resp2 := selectShare(t, "FILE01")
	tok2 := "&token=" + url.QueryEscape(resp2.Data.Token)
	status, body = performDownloadPath(t, "code=FILE01"+tok2)
	require.Equal(t, 200, status, "第二次下载应成功：%s", body)

	remaining, used := usedCount(t, g, "FILE01")
	assert.Equal(t, 3, remaining, "两次下载各扣 1 次")
	assert.Equal(t, 2, used)
}

// 文件分享（令牌关闭的降级部署）：无「下载会话」凭证 ⇒ 兜底键 (分享码, 取件人 IP)
// ⇒ 同一收件人的多次下载算**同一次取件**（只扣 1 次）。
//
// 这是降级模式的取舍：宁可少扣（同一收件人反复下载不重复吃额度），
// 也不要「逐个文件下载逐个扣」把额度吃光（那是本次重构前的缺陷）。
func TestDownload_FileShare_NoToken_FallbackChargesOncePerViewer(t *testing.T) {
	g := newChargeTestEnv(t, false) // 令牌关闭
	insertFileCode(t, g, &model.FileCode{
		Code: "FILE02", Text: "a.bin", FilePath: "uploads", UUIDFileName: "a.bin",
		Size: 10, ExpiredCount: 5,
	})

	status, body := performDownloadPath(t, "code=FILE02")
	require.Equal(t, 200, status, "下载应成功：%s", body)
	status, body = performDownloadPath(t, "code=FILE02")
	require.Equal(t, 200, status, "第二次下载应成功：%s", body)

	remaining, used := usedCount(t, g, "FILE02")
	assert.Equal(t, 4, remaining, "无令牌时同一收件人只扣 1 次")
	assert.Equal(t, 1, used)
}
