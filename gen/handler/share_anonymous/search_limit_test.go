// 匿名 search 面的「次数耗尽」回归（2026-10-24）
//
// 缺陷：model.FileCode.IsExpired() 拆分前含次数语义（ExpiredCount==0 ⇒ 过期）
// ⇒ /anonymous/search 在额度用完后报「取件码不存在」（404）。拆分后 IsExpired()
// 只判时间 ⇒ 本面**漏掉了次数闸** ⇒ 额度用完后仍回显文件名/大小/取件统计
// （分享已失效但元信息仍可读）。旧口径是 404 ⇒ 拆分属回归。
//
// 口径：耗尽给独立错误码 20007「取件码已用完」（与 /anonymous/retrieve、
// /anonymous/download 同码），不再伪装成「不存在/已过期」。
//
// 断言必须能变红：把 SearchByCode 里的 IsCountExhausted 闸门删掉，
// 本用例即失败。
package share_anonymous

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/model"
)

// performSearch 调 GET /anonymous/search/:code，返回 HTTP 码与响应体。
func performSearch(t *testing.T, code string) (int, string) {
	t.Helper()
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.GET("/anonymous/search/:code", SearchByCode)

	w := ut.PerformRequest(h.Engine, "GET", "/anonymous/search/"+url.PathEscape(code), nil,
		ut.Header{Key: "Host", Value: "test.local"})
	return w.Code, w.Body.String()
}

// bizCode 解析响应体里的业务码。
func bizCode(t *testing.T, body string) int {
	t.Helper()
	var r struct {
		Code int `json:"code"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &r), "响应体应可解析：%s", body)
	return r.Code
}

// TestSearchByCode_CountExhausted 额度用完后，search 面给 20007 且不回显元信息。
func TestSearchByCode_CountExhausted(t *testing.T) {
	g, code := newAnonFlowEnv(t, 1)

	// 正向对照：额度未用完 ⇒ 回显元信息
	httpCode, body := performSearch(t, code)
	require.Equal(t, 200, httpCode, "额度未用完时 search 应成功：%s", body)
	assert.Equal(t, 0, bizCode(t, body), "额度未用完时业务码应为 0：%s", body)

	// 把剩余次数扣到 0（模拟额度用完）
	require.NoError(t, g.Model(&model.FileCode{}).
		Where("code = ?", "ANON01").
		Update("expired_count", 0).Error)

	// 负向：必须是 20007「取件码已用完」，不是 20005「不存在」也不是 20006「已过期」
	_, body = performSearch(t, code)
	assert.Equal(t, 20007, bizCode(t, body),
		"额度用完应是 20007「取件码已用完」，不得伪装成不存在/已过期：%s", body)
	assert.NotContains(t, body, anonBody, "额度用完后不得回显文件内容")
}
