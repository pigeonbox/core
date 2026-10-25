package baseurl

import "strings"

// 分享链接形态的唯一真相源（2026-10-22 真机定位的「死链」修复）。
//
// 本产品前端用 hash 路由（createWebHashHistory，见 frontend-core
// src/router/index.ts）⇒ **页面地址里的路由段在 '#' 之后**。
//
// 页面链接与直链是两种东西，形态不可混用：
//
//	页面链接（人点开看内容）  {base}/#/share/{code}
//	直链（服务端要校验令牌）  {base}/share/download?code={code}&token={token}
//
// 为什么路径形态 {base}/share/{code} 打不开（2026-10-22 三台真机实测）：
//   - docker 前后端分离：前端容器 nginx 把 /share/* 反代给后端（nginx.conf
//     的通用 API 正则含 share），后端**没有**这条页面路由 ⇒ 硬 404。
//   - 原生单进程（fnOS/QNAP/openwrt）：后端 NoRoute 回退 SPA index.html，
//     但 index.html 用相对资源路径（./assets/…）⇒ 浏览器按 /share/ 解析 ⇒
//     资源取不到（拿到的是 SPA fallback 的 text/html，MIME 校验拒绝执行）⇒
//     **整页白屏**。
//
// Web UI 自己构造的链接一直是 hash 形态（ShareResultDialog / Dashboard /
// Shares 三处），**只有后端 API 下发的是路径形态** ⇒ MCP 工具、脚本、IM
// 机器人拿到的分享链接全是死链（前端有转换兜底，后端没有）。
//
// ⚠️ 令牌绝不能落在 '#' 之后：服务端收不到 ⇒ 直链必须走 query。
// 这是本页把「页面链接」与「直链」拆成两个函数的原因，不要合并。

// SharePageURL 拼「用户可点击的取件页」地址：{base}/#/share/{code}。
//
// base 为空（未配置 base_url 且请求来源不可推断）时退相对形态
// "/#/share/{code}"，由浏览器端按 location.origin 补全。
func SharePageURL(base, code string) string {
	return strings.TrimSuffix(base, "/") + "/#/share/" + code
}

// ShareDownloadURL 拼「带下载令牌的直链」：{base}/share/download?code={code}[&token={token}]。
//
// 与 SharePageURL 的分工：这里的令牌要**由服务端校验**，所以必须是 query
// 形态（落在 '#' 之后服务端就收不到，2026-10-22 修复时的关键约束）。
// 与取件详情响应下发的 url 字段同构（core 取件链路已用该形态）。
func ShareDownloadURL(base, code, token string) string {
	u := strings.TrimSuffix(base, "/") + "/share/download?code=" + code
	if token != "" {
		u += "&token=" + token
	}
	return u
}

// SplitSharePageURL 从页面链接里拆出 base 与分享码。
//
// 用途：由服务端下发的分享链接派生直链（base + /share/download?code=…）。
//
// 形态：**只有** {base}/#/share/{code}（hash 路由）——服务端自产的链接一律是它
// （SharePageURL 的唯一产物；MCP 与 presign complete 的两个调用方传的都是
// 服务端自产 URL）⇒ 旧形态 {base}/share/{code} 分支**不可达**，已移除。
//
// base 为空（相对形态）时返回空 base，调用方拼出相对直链。
func SplitSharePageURL(pageURL, code string) (base string) {
	suffix := "#/share/" + code
	if strings.HasSuffix(pageURL, suffix) {
		// 相对形态 "/#/share/CODE" 剥完剩 "/" ⇒ 归一成 ""（否则拼直链会出双斜杠）
		return strings.TrimSuffix(strings.TrimSuffix(pageURL, suffix), "/")
	}
	return pageURL
}
