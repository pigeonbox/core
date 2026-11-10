package user

// core#119 wire 判据：/user/password/forgot 的邮箱格式非法 ⇒ 400 + 10001
// （全站信封）；合法格式（含未知邮箱）维持 200 恒成功（防账号枚举）。
//
// 负对照（已实跑）：把 ForgotPassword 里 RequestPasswordReset 的错误分支
// 改回下划线吞掉（_ =）⇒ 400 用例转红（变成 200）。
//
// 为什么在生成物目录下加测试：gen/handler 的 Gen drift check 对 gen/handler
// 只对账文件增删，而 CHECK=1 沙箱会整目录快照既有文件 ⇒ 新增测试文件两边
// 清单一致，不触发漂移（与 user_trust.go 等手写文件同款处置）。

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

	userapp "github.com/pigeonbox/core/app/user"
	"github.com/pigeonbox/core/repo/db/model"
)

func TestForgotPassword_MalformedEmail400(t *testing.T) {
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(
		&model.User{}, &model.UserAPIKey{}, &model.FileCode{},
		&model.UserSession{}, &model.PasswordResetToken{},
	))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1) // :memory: 每连接独立库，钉单连接防 flake
	SetUserService(userapp.NewService(g))

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.POST("/user/password/forgot", ForgotPassword)

	post := func(email string) (int, string) {
		t.Helper()
		body, merr := json.Marshal(map[string]string{"email": email})
		require.NoError(t, merr)
		w := ut.PerformRequest(h.Engine, http.MethodPost, "/user/password/forgot",
			&ut.Body{Body: strings.NewReader(string(body)), Len: len(body)},
			ut.Header{Key: "Content-Type", Value: "application/json"},
			ut.Header{Key: "Host", Value: "test.local"})
		return w.Code, w.Body.String()
	}

	// 注入类输入 ⇒ 400 + 10001（issue 守卫判据）
	code, body := post("x@y.com\r\nBcc:evil@evil.com")
	assert.Equal(t, http.StatusBadRequest, code, "注入类邮箱必须 400: %s", body)
	assert.Contains(t, body, "\"code\":10001", "必须是参数错误业务码: %s", body)

	// display-name 形态同样拒绝
	code, body = post("Name <x@y.com>")
	assert.Equal(t, http.StatusBadRequest, code, body)

	// 合法格式（含未知邮箱）⇒ 200 恒成功（防账号枚举语义不动）
	code, body = post("nobody@example.com")
	assert.Equal(t, http.StatusOK, code, body)
	assert.Contains(t, body, "\"code\":0", body)
}
