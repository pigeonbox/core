// core#137 wire 判据：/admin/stats 与 /admin/config 在坏库下必须走 500 +
// 全站信封（resp.NewInternalError），不得 200 回零值统计/默认配置。
//
// 修复前：GetStats 逐项吞错恒 return nil、GetConfig 装载失败静默回退默认值
// ⇒ 坏库下两个端点都是 200 假成功（统计页全 0、配置页显示默认配置，无错误态）。
//
// 负对照（已实跑）：GetStats 回退吞错 / GetConfig 门面去掉 LoadError 检查
// ⇒ 本判据转红（200 假成功回归）。
package admin

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pigeonbox/contracts/errcode"
)

func TestAdminStatsConfig_FailVisibleOnBrokenDB(t *testing.T) {
	newBrokenAdminEnv(t)

	for _, tc := range []struct{ method, path, what string }{
		{"GET", "/admin/stats", "AdminStats"},
		{"GET", "/admin/config", "AdminGetConfig"},
	} {
		code, body := doAdminErrReq(t, tc.method, tc.path, "")
		assert.Equalf(t, http.StatusInternalServerError, code,
			"%s 坏库必须 500（不得 200 假成功）：%v", tc.what, body)
		assertAdminErrEnvelope(t, body, errcode.CodeInternal, "no such table", "SELECT", "sql:")
	}
}
