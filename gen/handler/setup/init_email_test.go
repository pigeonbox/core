// core#119 同族 wire 判据：POST /setup 的 admin_email 格式非法 ⇒ 400 + 10001
// 全站信封（消息点名 admin_email、带 trace_id），不落 500「系统初始化失败」。
//
// 负对照（已实跑）：删掉 gen/handler/setup Initialize 的 AppError 分支（或
// app/setup 的 ValidateEmail 闸）⇒ 400 用例转红（变成 500 或200）。
package setup

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

	setupservice "github.com/pigeonbox/core/app/setup"
	"github.com/pigeonbox/core/repo/db/model"
)

func TestInitialize_MalformedAdminEmail400(t *testing.T) {
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.User{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	SetSetupService(setupservice.NewService(g))

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.POST("/setup", Initialize)

	post := func(email string) (int, string) {
		t.Helper()
		body, merr := json.Marshal(map[string]string{
			"admin_username": "admin1", "admin_password": "secret123!", "admin_email": email,
		})
		require.NoError(t, merr)
		w := ut.PerformRequest(h.Engine, http.MethodPost, "/setup",
			&ut.Body{Body: strings.NewReader(string(body)), Len: len(body)},
			ut.Header{Key: "Content-Type", Value: "application/json"},
			ut.Header{Key: "Host", Value: "test.local"})
		return w.Code, w.Body.String()
	}

	// 注入类邮箱 ⇒ 400 + 10001 全站信封（点名 admin_email）
	code, body := post("x@y.com\r\nBcc:evil@evil.com")
	assert.Equal(t, http.StatusBadRequest, code, "注入类邮箱必须 400: %s", body)
	assert.Contains(t, body, "\"code\":10001", "必须是参数错误业务码: %s", body)
	assert.Contains(t, body, "admin_email", "消息应点名参数: %s", body)
	assert.Contains(t, body, "trace_id", "全站信封应带 trace_id: %s", body)

	// display-name 形态同拒
	code, body = post("Name <x@y.com>")
	assert.Equal(t, http.StatusBadRequest, code, body)

	// 合法 ⇒ 200（收口只拦格式，不动正常初始化）
	code, body = post("admin@example.com")
	assert.Equal(t, http.StatusOK, code, "合法邮箱应正常初始化: %s", body)
}
