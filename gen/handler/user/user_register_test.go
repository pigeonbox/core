// /user/register 的**错误映射**边界断言（2026-11-02 修复注册撞名 500 后补守卫）。
//
// 背景：Create 返回普通 error ⇒ handler 一律映射 500/CodeInternal ⇒
// 注册撞名在前端只显示「服务器内部错误」，用户无从得知该换用户名。
// 修复口径：撞名/撞邮箱 = 资源冲突（409 + 10016）；用户名格式非法 =
// 参数错误（400 + 10001）；正常注册 = 200 + code 0。
//
// 断言必须能变红（负对照）：
//
//	· 把 handler 的 409 映射改回 500 ⇒「撞名/撞邮箱」两用例红（期望 409 得 500）；
//	· 把 service 的 sentinel 换回 errors.New 裸错误 ⇒ 撞名用例红
//	  （errors.Is 不命中 ⇒ 落 typed/500 分支）；
//	· 把 ValidateUsername 的 typed 包装去掉 ⇒ 非法用户名用例红（400 变 500）。
package user

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
	"github.com/pigeonbox/core/conf"
)

// doRegisterReq 打 /user/register（该路由不在 doUserReq 的登录专属清单里，
// 注册面是匿名面，单独走一个不带身份注入的台架）。
func doRegisterReq(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.POST("/user/register", Register)

	w := ut.PerformRequest(h.Engine, "POST", "/user/register",
		&ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// TestRegister_ErrorMapping 注册面四条主路径的 wire 形状：
// 正常 200 / 撞用户名 409 / 撞邮箱 409 / 非法用户名 400。
func TestRegister_ErrorMapping(t *testing.T) {
	newUserHandlerEnv(t)

	// 打开注册开关（RegistrationAllowed：defaults 未注入时读全局 conf）
	conf.SetGlobalConfig(&conf.AppConfiguration{
		User: conf.UserConfig{AllowUserRegistration: true},
	})
	t.Cleanup(func() { conf.SetGlobalConfig(nil) })

	// 正常注册 ⇒ 200 + code 0
	code, body := doRegisterReq(t,
		`{"username":"alice","email":"alice@example.com","password":"secret123","nickname":"A"}`)
	require.Equal(t, http.StatusOK, code, "正常注册应 200：%v", body)
	assert.EqualValues(t, 0, body["code"], "成功响应业务码应为 0：%v", body)

	// 撞用户名 ⇒ 409 + 10016（不得 500/CodeInternal）
	code, body = doRegisterReq(t,
		`{"username":"alice","email":"other@example.com","password":"secret123","nickname":"B"}`)
	assert.Equal(t, http.StatusConflict, code,
		"撞用户名应 409（不是 500「服务器内部错误」）：%v", body)
	assert.EqualValues(t, errcode.CodeConflict, body["code"],
		"撞用户名业务码应为 %d：%v", errcode.CodeConflict, body)

	// 撞邮箱 ⇒ 409 + 10016
	code, body = doRegisterReq(t,
		`{"username":"another","email":"alice@example.com","password":"secret123","nickname":"C"}`)
	assert.Equal(t, http.StatusConflict, code, "撞邮箱应 409：%v", body)
	assert.EqualValues(t, errcode.CodeConflict, body["code"],
		"撞邮箱业务码应为 %d：%v", errcode.CodeConflict, body)

	// 非法用户名（空格/符号不在白名单）⇒ 400 + 10001（不得 500）
	code, body = doRegisterReq(t,
		`{"username":"bad name!","email":"bad@example.com","password":"secret123","nickname":"D"}`)
	assert.Equal(t, http.StatusBadRequest, code, "非法用户名应 400：%v", body)
	assert.EqualValues(t, errcode.CodeInvalidParam, body["code"],
		"非法用户名业务码应为 %d：%v", errcode.CodeInvalidParam, body)
}

// TestRegister_Closed_403 注册关闭时是 403（既有行为锁定，防错误映射改造
// 顺手把注册开关的语义改坏）。
func TestRegister_Closed_403(t *testing.T) {
	newUserHandlerEnv(t)

	conf.SetGlobalConfig(&conf.AppConfiguration{
		User: conf.UserConfig{AllowUserRegistration: false},
	})
	t.Cleanup(func() { conf.SetGlobalConfig(nil) })

	code, body := doRegisterReq(t,
		`{"username":"alice","email":"alice@example.com","password":"secret123","nickname":"A"}`)
	assert.Equal(t, http.StatusForbidden, code, "注册关闭应 403：%v", body)
	assert.EqualValues(t, errcode.CodeForbidden, body["code"], "%v", body)
}
