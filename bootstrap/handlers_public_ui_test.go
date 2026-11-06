package bootstrap

// /api/config 的 **ui 段**下发守卫（2026-11-06）。
//
// ## 为什么要有
//
// 线上实录（4.9.0，10.10.10.3）：管理台把主题色改成 #409eff、把管理入口
// 改成自定义路径、关掉 API 文档后，前端**完全不生效**。现场取证：
// system_configs 里 ui = {accent_color:"#409eff", expose_openapi:false,
// show_admin_addr:true, admin_path:""} 已持久化，但 GET /api/config 返回
// accentColor:"" / apiDocsEnabled:true / adminPath:"admin"。
//
// 根因：本 handler 的 UI 段直读**包级 config**，而 2026-10-31 全局配置改为
// copy-on-write 换指针后，`config` 指向的是**被换掉的旧对象** ⇒ 管理台持久化
// 的 overlay 永远读不到（同函数里 name/description/download 走
// configApp.GetConfig 所以正常，唯独 UI 段分裂）。
//
// ## 守卫点（每个都有牙齿：回退成直读旧指针即红）
//
//  1. accentColor / background 取持久化值（旧指针里是"另一个值"，不是空）；
//  2. adminPath 取持久化值（旧指针回退 "admin"）；
//  3. apiDocsEnabled 取持久化值（旧指针给零值 false，此处持久化为 true）；
//  4. showAdminAddr 取持久化值（旧指针 nil ⇒ 视为展示=true，此处持久化 false）。

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

func TestPublicConfigHandler_UISectionFollowsPersistedConfig(t *testing.T) {
	ctx := context.Background()

	gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, gormDB.AutoMigrate(
		&model.SystemConfigRecord{}, &model.User{}, &model.AdminOperationLog{}))
	sqlDB, err := gormDB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	// 配置中心是进程级单例（首次 InitDefault 生效）：本包可能已有其它用例装配过，
	// 已装配就直接用它的库——本用例只关心「持久化的 ui 段会不会被下发」。
	if configApp.DefaultSafe() == nil {
		configApp.InitDefault(gormDB)
	}

	oldSetup := publicSetupSvc
	publicSetupSvc = setupApp.NewService(gormDB)
	t.Cleanup(func() { publicSetupSvc = oldSetup })

	// 全局 conf：干净起点（handler 里 oidcEnabled 等直读它）
	oldConf := conf.GetGlobalConfig()
	conf.SetGlobalConfig(&conf.AppConfiguration{})
	t.Cleanup(func() { conf.SetGlobalConfig(oldConf) })

	// **旧对象**：模拟 COW 换指针之后残留的包级 config——里面是"过期值"，
	// 且刻意与持久化值不同，这样"读了旧对象"会被断言抓到。
	oldConfig := config
	config = &conf.AppConfiguration{
		UI: conf.UIConfig{
			AccentColor:   "#111111",
			AdminPath:     "oldpanel",
			ExposeOpenAPI: false,
			Background:    "https://stale.example/bg.png",
			ShowAdminAddr: boolPtrPublic(true),
		},
	}
	t.Cleanup(func() { config = oldConfig })

	// 管理台保存（真实写入路径：写库 + 更新内存 + overlay 到全局 conf）
	showAdmin := false
	require.NoError(t, configApp.Default().UpdateConfig(ctx, &configApp.SystemConfig{
		UI: &conf.UIConfig{
			AccentColor:   "#409eff",
			AdminPath:     "console",
			ExposeOpenAPI: true,
			Background:    "https://cdn.example/bg.png",
			ShowAdminAddr: &showAdmin,
		},
	}))

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.GET("/api/config", publicConfigHandler)
	w := ut.PerformRequest(h.Engine, "GET", "/api/config", nil,
		ut.Header{Key: "Host", Value: "test.local"})
	require.Equal(t, 200, w.Code)

	var out struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))

	assert.Equal(t, "#409eff", out.Data["accentColor"],
		"主题色必须取管理台持久化值（读旧指针会得到 #111111，不改代码前是空串）")
	assert.Equal(t, "https://cdn.example/bg.png", out.Data["background"],
		"背景图必须取持久化值")
	assert.Equal(t, "console", out.Data["adminPath"],
		"管理入口路径必须取持久化值（读旧指针会回退 admin）")
	assert.Equal(t, true, out.Data["apiDocsEnabled"],
		"API 文档开关必须取持久化值（读旧指针会得到零值 false）")
	assert.Equal(t, false, out.Data["showAdminAddr"],
		"管理入口可见性必须取持久化值（读旧指针为 nil ⇒ true）")
}

func boolPtrPublic(v bool) *bool { return &v }
