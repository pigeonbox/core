// core#139 wire 判据：/admin/stats/enhanced 在坏库下必须 500 + 全站信封
// （resp.NewInternalError），不得 200 回全 0 富指标。
//
// 修复前：EnhancedStats 单项吞错恒成功 ⇒ 坏库下 200 假成功（Dashboard 富指标
// 全 0、无错误态）。
//
// 负对照（实做）：EnhancedStats 回退 `stats.X, _ = …` 吞错 ⇒ 本判据转红。
package admin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/contracts/errcode"
)

func TestAdminEnhancedStats_FailVisibleOnBrokenDB(t *testing.T) {
	newBrokenManageEnv(t)
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.GET("/admin/stats/enhanced", AdminEnhancedStats)

	w := ut.PerformRequest(h.Engine, "GET", "/admin/stats/enhanced",
		&ut.Body{Body: strings.NewReader(""), Len: 0},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, http.StatusInternalServerError, w.Code,
		"坏库必须 500（不得 200 全 0 假成功）：%v", body)
	assertAdminErrEnvelope(t, body, errcode.CodeInternal, "no such table", "SELECT", "sql:")
}
