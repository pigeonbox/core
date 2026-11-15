package admin

// policy_template_test.go 策略模板端点分流验收（hub#368）：
//   - GET /admin/config 下发 policy_templates 顶层键（宽松响应面复用，contracts 不动）
//   - PUT /admin/config {"policy_template": "<id>"} 走模板 apply
//   - 混提交/非字符串/未知模板 ⇒ 400 + 10001 + 可操作文案（不进 200 假成功）

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

	adminsvc "github.com/pigeonbox/core/app/admin"
	"github.com/pigeonbox/core/repo/db/model"
)

// newWorkingAdminEnv admin service 绑到**建好表**的内存库（与 newBrokenAdminEnv 相对）。
func newWorkingAdminEnv(t *testing.T) {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.SystemConfigRecord{}, &model.AdminOperationLog{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1) // glebarez :memory: 每连接独立库，钉单连接防 flake
	adminService = adminsvc.NewService(g)
	t.Cleanup(func() { adminService = nil })
}

// doPolicyReq 打一次 PUT /admin/config（与 error_envelope 同款直挂 handler）。
func doPolicyReq(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.PUT("/admin/config", AdminUpdateConfig)
	h.GET("/admin/config", AdminGetConfig)
	w := ut.PerformRequest(h.Engine, "PUT", "/admin/config",
		&ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// TestAdminUpdateConfig_PolicyTemplateApply 模板应用成功路径：200 + code 0，
// 且配置真的落库生效（不是假成功）。
func TestAdminUpdateConfig_PolicyTemplateApply(t *testing.T) {
	newWorkingAdminEnv(t)

	code, body := doPolicyReq(t, `{"policy_template":"intranet"}`)
	assert.Equal(t, http.StatusOK, code, "%v", body)
	assert.Equal(t, float64(0), body["code"], "%v", body)

	// 回读确认落库生效
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.GET("/admin/config", AdminGetConfig)
	w := ut.PerformRequest(h.Engine, "GET", "/admin/config", &ut.Body{Len: 0})
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	require.Equal(t, float64(0), out["code"], "%v", out)
	data, ok := out["data"].(map[string]any)
	require.True(t, ok, "data 应是对象: %v", out)
	uploadEx, ok := data["upload_ex"].(map[string]any)
	require.True(t, ok, "intranet 应写入 upload_ex: %v", data)
	assert.Equal(t, true, uploadEx["open_upload"], "模板应用后 open_upload 应为 true")
}

// TestAdminGetConfig_PolicyTemplatesList GET 下发模板清单顶层键（含三个内置 ID）。
func TestAdminGetConfig_PolicyTemplatesList(t *testing.T) {
	newWorkingAdminEnv(t)

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.GET("/admin/config", AdminGetConfig)
	w := ut.PerformRequest(h.Engine, "GET", "/admin/config", &ut.Body{Len: 0})
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)

	require.Equal(t, float64(0), out["code"], "%v", out)
	raw, ok := out["policy_templates"]
	require.True(t, ok, "GET /admin/config 必须下发 policy_templates 顶层键: %v", out)
	list, ok := raw.([]any)
	require.True(t, ok, "policy_templates 应是数组: %T", raw)
	require.Len(t, list, 3, "三个内置模板")
	ids := map[string]bool{}
	for _, one := range list {
		m := one.(map[string]any)
		ids[m["id"].(string)] = true
	}
	assert.True(t, ids["intranet"])
	assert.True(t, ids["public-no-anonymous"])
	assert.True(t, ids["enterprise"])
}

// TestAdminUpdateConfig_PolicyTemplateParamErrors 参数类 400 三条：
// 混提交 / 非字符串 / 未知模板（+ 10001 + 可操作文案，不进 200 假成功）。
func TestAdminUpdateConfig_PolicyTemplateParamErrors(t *testing.T) {
	newWorkingAdminEnv(t)

	cases := []struct {
		name string
		body string
		want string
	}{
		{"混提交", `{"policy_template":"intranet","base":{"name":"x"}}`, "单独提交"},
		{"非字符串", `{"policy_template":123}`, "非空字符串"},
		{"未知模板", `{"policy_template":"no-such"}`, "未知策略模板"},
		{"空串", `{"policy_template":""}`, "非空字符串"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doPolicyReq(t, tc.body)
			assert.Equal(t, http.StatusBadRequest, code, "参数类必须 400: %v", body)
			assert.Equal(t, float64(10001), body["code"], "%v", body)
			msg, _ := body["message"].(string)
			assert.Contains(t, msg, tc.want, "拒绝文案必须点名问题: %v", body)
			assert.NotEmpty(t, body["trace_id"], "错误信封必须带 trace_id: %v", body)
		})
	}
}
