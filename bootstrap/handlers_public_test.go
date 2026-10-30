package bootstrap

// /api/config 的上传体积口径断言（2026-10-30）。
//
// ## 为什么要有这份测试
//
// 2026-10-30 事故：前端只从 /api/config 拿到**单文件**上限（upload.max_file_size
// = 4GB）就显示「单文件最大 4 GB」，而登录用户实际被**另一个**闸挡住——
// 「单次发送**总量**上限」（user.user_upload_size，出厂默认 50MB）⇒
// 服务端返回 413「上传总大小超过限制（上限 52428800 字节）」，
// 与 UI 显示的 4GB 完全对不上。
//
// ## 守卫点（三个）
//
//  1. **两个闸一起下发**：maxFileSize（单文件）+ maxBatchSize（单次发送合计）——
//     只加一个仍会让 UI 与真实限制对不上（正是事故形态）。
//  2. **取值口径**：管理台持久化的 user 段优先（与 GetUploadSizeCap 的系统默认层同源）。
//  3. **不泄露 per-user 值**：公开面只放站点级默认；登录用户私有上限
//     （users.max_upload_size）以服务端 413 为准。

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	configApp "github.com/pigeonbox/core/app/config"
	setupApp "github.com/pigeonbox/core/app/setup"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/repo/db/model"
)

func TestPublicConfigHandler_UploadCaps(t *testing.T) {
	ctx := context.Background()

	// 装配全站配置中心单例（system_configs 单行 JSON）。
	// ⚠️ InitDefault 是**进程级单例**（首次装配生效）——本包无其他测试碰它，
	// 故此处首次调用即为本包唯一数据源；下面前置断言会把「被别的测试抢先装配」
	// 这类情况显式报出来，而不是让取值断言含义不明。
	gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, gormDB.AutoMigrate(&model.SystemConfigRecord{}, &model.User{}))
	sqlDB, err := gormDB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1) // glebarez/sqlite :memory: 多连接各为独立库，钉死单连接防 flake
	require.NoError(t, gormDB.Create(&model.SystemConfigRecord{Data: `{"user":{"allowuserregistration":false,"useruploadsize":268435456,"userstoragequota":1073741824,"sessionexpiryhours":168}}`}).Error)
	configApp.InitDefault(gormDB)
	require.Equal(t, int64(268435456), configApp.Default().EffectiveUploadBatchCap(ctx),
		"配置中心单例须由本用例装配（InitDefault 首次生效）")

	// handler 读 conf.GetGlobalConfig().Security.OIDC ⇒ 全局 conf 不可为 nil
	oldConf := conf.GetGlobalConfig()
	conf.SetGlobalConfig(&conf.AppConfiguration{})
	t.Cleanup(func() { conf.SetGlobalConfig(oldConf) })

	// 本包级单例：/api/config 的初始化状态来源 + 站点配置
	oldSetup := publicSetupSvc
	publicSetupSvc = setupApp.NewService(gormDB)
	t.Cleanup(func() { publicSetupSvc = oldSetup })

	oldConfig := config
	config = &conf.AppConfiguration{
		Upload: conf.UploadConfig{MaxFileSize: 1073741824}, // 单文件上限 1GB
	}
	t.Cleanup(func() { config = oldConfig })

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.GET("/api/config", publicConfigHandler)
	w := ut.PerformRequest(h.Engine, "GET", "/api/config", nil,
		ut.Header{Key: "Host", Value: "test.local"})
	require.Equal(t, 200, w.Code)

	var out struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))

	assert.EqualValues(t, 1073741824, out.Data["maxFileSize"], "单文件上限必须下发")
	assert.EqualValues(t, 268435456, out.Data["maxBatchSize"],
		"单次发送总量上限必须下发（与单文件上限是两个闸，只发一个必然对不上）")

	// per-user 私有值不得出现在公开面（登录用户的实际上限以服务端 413 为准）
	body := w.Body.String()
	assert.NotContains(t, body, "useruploadsize", "内部字段名不得出现在公开响应")
	assert.NotContains(t, body, "max_upload_size", "per-user 私有值不得出现在公开响应")
}
