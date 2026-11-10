// core#114 A6b：gen/handler/admin/admin_manage.go 错误出口收编的 wire 判据。
//
// 判据：
//   - 内部（DB）故障 ⇒ 500 + 全站信封 + 内部细节不出网（此前部分 catch-all
//     用 40001/40002 误码 ⇒ 落 404/**HTTP 200**，且 message 带 SQL 原文）；
//   - AdminCreateUser 内部故障不得进 HTTP 200（core#115 主判据同族）。
//
// 负对照（已实跑）：把 AdminListUsersFiltered 错误分支回退为
// NewErrorWithMessage(c, 50001, "查询文件列表失败: "+err.Error()) ⇒ 转红。
package admin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	adminapp "github.com/pigeonbox/core/app/admin"
	userapp "github.com/pigeonbox/core/app/user"
)

// newBrokenManageEnv manageSvc/manageUserSvc 绑未建表内存库。
func newBrokenManageEnv(t *testing.T) {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	SetManageServices(adminapp.NewService(g), userapp.NewService(g))
	t.Cleanup(func() { SetManageServices(nil, nil) })
}

func TestAdminManageErrorEnvelope_InternalSanitized(t *testing.T) {
	newBrokenManageEnv(t)
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.GET("/admin/users/filter", AdminListUsersFiltered)
	h.GET("/admin/stats/enhanced", AdminEnhancedStats)
	h.POST("/admin/users", AdminCreateUser)

	for _, tc := range []struct{ method, path, body, what string }{
		{"GET", "/admin/users/filter?page=1", "", "AdminListUsersFiltered"},
		{"POST", "/admin/users", `{"username":"uname","password":"pppppp"}`, "AdminCreateUser"},
	} {
		w := ut.PerformRequest(h.Engine, tc.method, tc.path,
			&ut.Body{Body: strings.NewReader(tc.body), Len: len(tc.body)},
			ut.Header{Key: "Content-Type", Value: "application/json"})
		var body map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		assert.Equal(t, http.StatusInternalServerError, w.Code,
			"%s 坏库故障应 500（业务错误/内部错误都不得进 HTTP 200）：%v", tc.what, body)
		assertAdminErrEnvelope(t, body, 10008, "no such table", "SELECT", "sql:")
	}
}
