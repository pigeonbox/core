// core#114 A6b：gen/handler/maintenance 错误出口收编的 wire 判据。
// 内部（DB）故障 ⇒ 500 + 10008 + 全站信封 + err 原文不出网。
// 负对照（已实跑）：把 CleanTempFiles 错误分支回退为 legacy map + err.Error() ⇒ 转红。
package maintenance

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/contracts/errcode"
	adminsvc "github.com/pigeonbox/core/app/admin"
)

func TestMaintenanceErrorEnvelope_InternalSanitized(t *testing.T) {
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	adminService = adminsvc.NewService(g)
	t.Cleanup(func() { adminService = nil })

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.POST("/clean-temp", CleanTempFiles)
	h.POST("/clean-expired", CleanExpiredFiles)

	for _, tc := range []struct{ path, what string }{
		{"/clean-temp", "CleanTempFiles"},
		{"/clean-expired", "CleanExpiredFiles"},
	} {
		w := ut.PerformRequest(h.Engine, "POST", tc.path, nil)
		var body map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		assert.Equal(t, http.StatusInternalServerError, w.Code,
			"%s 坏库故障应 500：%v", tc.what, body)
		assert.Equal(t, float64(errcode.CodeInternal), body["code"], "%v", body)
		assert.NotEmpty(t, body["trace_id"], "错误响应必须带 trace_id：%v", body)
		msg, _ := body["message"].(string)
		assert.NotEmpty(t, msg)
		for _, m := range []string{"no such table", "SELECT", "sql:"} {
			assert.NotContains(t, msg, m, "内部错误细节不得出网：%v", body)
		}
		keys := make([]string, 0, len(body))
		for k := range body {
			keys = append(keys, k)
		}
		assert.ElementsMatch(t, []string{"code", "message", "trace_id"}, keys,
			"错误响应应是全站信封：%v", body)
	}
}
