// 10 条 IDL 化业务路由的**响应形状回归**（2026-10-26）
//
// ## 为什么要有这份测试
//
// 2026-10-26 这批路由从「手写注册」收敛进 contracts IDL（路由由 hz 生成，
// 业务实现薄委托到 transport/http/handler）。收敛的**核心承诺**是：
// **wire 形状逐字节不变**（字段名、可空性、缺省行为、业务码、文案、HTTP 状态码）。
//
// 契约层（contracts）只能表达**结构**（字段名 + 类型），表达不了：
//
//	· 缺省字段是「省略」还是「显式 null」（可空性）；
//	· 成功响应里**根本没有 data 键**（logout）；
//	· 错误响应的业务码 + 中文文案 + HTTP 状态码的组合。
//
// ⇒ 这些一旦被改坏，**编译不报错、CI 也不一定红**（前端拿到的 JSON 少一个字段
// 或多一个 null，只在运行时表现）⇒ 必须有**逐字段**的机械断言。
//
// ## 覆盖的 5 条路由（10 条里挑可无副作用构造的）
//
//	GET  /share/metadata/:code        （取件元信息：交付面，字段最全）
//	POST /api/v1/user/refresh         （会话刷新：成功体 + 401 错误体）
//	POST /api/v1/user/logout          （登出：**成功体没有 data 键**）
//	GET  /api/v1/federation/resolve   （联邦解析：未启用时 available:false）
//	GET  /api/v1/notifies/public       （站级公开公告：条目 start_at/end_at 回 null）
//
// ## 断言方式
//
// 走**真实生成路由**（router.GeneratedRegister）⇒ 路径、中间件、handler 接线
// 一并被覆盖（不是只测 handler 函数）。
//
// 逐字段断言用「**键集合精确相等**」而不是「包含某字段」——
// 只断言包含 ⇒ 多出来的字段（契约没声明的）永远抓不到；
// 键集合相等 ⇒ 改名 / 增删 / 省略-vs-null 任一变化都会红。
//
// ## 负对照（必须能变红；三条都实跑过）
//
//	· 把 app/share/metadata.go 的 `json:"file_count"` 改成 `json:"filecount"`
//	  ⇒ TestIDLizedRouteResponseShape/share-metadata 必红（键集合不相等）。
//	· 把 transport/http/handler/session_ops.go 的 LogoutSession 成功体加上
//	  `"data": map[string]string{}` ⇒ logout 用例必红（多了一个 data 键）。
//	· 给 federation/resolve 的 data 多塞一个契约没声明的键（如 "debug_extra"）
//	  ⇒ federation-resolve 必红（键集合不相等 + 「暗字段不在契约模型里」）。
package bootstrap

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	commonmodel "github.com/pigeonbox/contracts/gen/common"
	publicapi "github.com/pigeonbox/contracts/gen/publicapi"
	"github.com/pigeonbox/core/app/notify"
	"github.com/pigeonbox/core/app/share"
	"github.com/pigeonbox/core/gen/router"
	"github.com/pigeonbox/core/pkg/auth"
	"github.com/pigeonbox/core/pkg/utils"
	"github.com/pigeonbox/core/repo/db/model"
	customHandler "github.com/pigeonbox/core/transport/http/handler"
)

// jsonKeys 返回 JSON 对象的**顶层键集合**（顺序无关）。
func jsonKeys(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &m), "响应不是合法 JSON 对象: %s", string(raw))
	return m
}

// assertKeysExact 断言顶层键集合**精确相等**（多一个 / 少一个 / 改名都红）。
func assertKeysExact(t *testing.T, raw []byte, want ...string) map[string]json.RawMessage {
	t.Helper()
	m := jsonKeys(t, raw)
	got := make([]string, 0, len(m))
	for k := range m {
		got = append(got, k)
	}
	assert.ElementsMatch(t, want, got, "响应顶层键集合不符：%s", string(raw))
	return m
}

// idlizedRouterServer 起一个只注册**生成路由**的 Hertz（不注册手写路由）。
// 走生成路由 ⇒ 路径 / 中间件 / handler 接线一并被覆盖。
func idlizedRouterServer() *server.Hertz {
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	router.GeneratedRegister(h)
	return h
}

// doRequest 打一次请求并返回状态码与响应体。
func doRequest(t *testing.T, h *server.Hertz, method, path string, headers ...ut.Header) (int, []byte) {
	t.Helper()
	w := ut.PerformRequest(h.Engine, method, path, nil, append([]ut.Header{
		{Key: "Host", Value: "test.local"},
	}, headers...)...)
	return w.Code, w.Body.Bytes()
}

// TestIDLizedRouteResponseShape 10 条 IDL 化路由的响应形状回归（逐字段）。
func TestIDLizedRouteResponseShape(t *testing.T) {
	h := idlizedRouterServer()

	// ===== 1. GET /share/metadata/:code —— 取件元信息（交付面，字段最全）=====
	t.Run("share-metadata", func(t *testing.T) {
		g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
		require.NoError(t, err)
		require.NoError(t, g.AutoMigrate(&model.FileCode{}, &model.FileCodeFile{}, &model.PickupCharge{}))
		sqlDB, err := g.DB()
		require.NoError(t, err)
		sqlDB.SetMaxOpenConns(1)

		svc := share.NewService(g, "http://localhost:12345", nil)
		customHandler.SetShareService(svc)
		t.Cleanup(func() { customHandler.SetShareService(nil) })

		// 文本分享、**永不过期**（ExpiredAt=nil）⇒ expired_at 显式回 null
		// （契约里是 optional string ⇒ 缺省不输出；此处钉住**线上现状**：
		//  收敛前是 *time.Time ⇒ null，收敛后仍是 null（薄委托，业务逻辑未动））
		created, err := svc.CreateShare(context.Background(), &share.ShareFileReq{
			Channel:      "direct",
			FilePath:     "",
			Size:         0,
			Text:         "机密内容ABC",
			ExpiredAt:    nil,
			ExpiredCount: -1,
			UploadType:   "anonymous",
		})
		require.NoError(t, err)

		code, body := doRequest(t, h, "GET", "/share/metadata/"+created.Code)
		require.Equal(t, http.StatusOK, code, "取件元信息应回 200：%s", string(body))

		top := assertKeysExact(t, body, "code", "message", "data", "trace_id")
		assert.Equal(t, "0", string(top["code"]))
		// ⚠️ 本端点走 resp.Success ⇒ message 是 "success"（**不是** "ok"：
		// 直接 c.JSON 的端点才是 "ok"）——两套文案都钉住，防被"统一"时无声漂移。
		assert.Equal(t, `"success"`, string(top["message"]))

		data := jsonKeys(t, top["data"])
		// ⚠️ 键集合**精确相等**：文本分享没有文件名 ⇒ name 必须**不出现**；
		// expired_at 必须是**显式 null**（不是省略、不是 ""）。
		assert.ElementsMatch(t, []string{
			"code", "type", "size", "has_password", "expired_at",
			"expired_count", "used_count", "encrypted", "file_count", "created_at",
		}, keysOf(data), "取件元信息字段集不符：%s", string(top["data"]))
		// 契约（publicapi.ShareMetadataData）↔ 线上形状：required 一个不少、
		// 暗字段一个不多。name 是 optional ⇒ 文本分享缺席不算违规。
		assertMatchesContract(t, data, publicapi.ShareMetadataData{}, "取件元信息 data")
		assert.Equal(t, `"text"`, string(data["type"]))
		assert.Equal(t, "null", string(data["expired_at"]), "expired_at 应显式回 null")
		assert.Equal(t, "-1", string(data["expired_count"]), "永不过期 ⇒ expired_count=-1")

		// 不存在的码 ⇒ 404 + 业务码 10004 + 原文案
		code, body = doRequest(t, h, "GET", "/share/metadata/ZZZZZZ")
		require.Equal(t, http.StatusNotFound, code, "不存在的码应回 404：%s", string(body))
		assertKeysExact(t, body, "code", "message")
		assert.JSONEq(t, `{"code":10004,"message":"分享不存在或已过期"}`, string(body))
	})

	// ===== 2. POST /api/v1/user/refresh —— 会话刷新 =====
	t.Run("user-refresh", func(t *testing.T) {
		// 缺令牌 ⇒ 401 + 10002 + 原文案（业务码/文案/状态码三件套）
		code, body := doRequest(t, h, "POST", "/api/v1/user/refresh")
		require.Equal(t, http.StatusUnauthorized, code, "缺令牌应回 401：%s", string(body))
		assertKeysExact(t, body, "code", "message")
		assert.JSONEq(t, `{"code":10002,"message":"missing token"}`, string(body))

		// 正常刷新 ⇒ 200 + data.token
		token, err := auth.GenerateToken(7, "alice", "user")
		require.NoError(t, err)
		code, body = doRequest(t, h, "POST", "/api/v1/user/refresh",
			ut.Header{Key: "Authorization", Value: "Bearer " + token})
		require.Equal(t, http.StatusOK, code, "有效令牌应回 200：%s", string(body))

		top := assertKeysExact(t, body, "code", "message", "data")
		assert.Equal(t, "0", string(top["code"]))
		assert.Equal(t, `"ok"`, string(top["message"]))
		data := jsonKeys(t, top["data"])
		assert.ElementsMatch(t, []string{"token"}, keysOf(data), "refresh 响应 data 字段集不符：%s", string(top["data"]))
		assertMatchesContract(t, data, commonmodel.UserRefreshData{}, "refresh 响应 data")
		assert.NotEmpty(t, string(data["token"]))
	})

	// ===== 3. POST /api/v1/user/logout —— 登出（成功体**没有 data 键**）=====
	t.Run("user-logout", func(t *testing.T) {
		// 缺令牌 ⇒ 401
		code, body := doRequest(t, h, "POST", "/api/v1/user/logout")
		require.Equal(t, http.StatusUnauthorized, code, "缺令牌应回 401：%s", string(body))
		assertKeysExact(t, body, "code", "message")
		assert.JSONEq(t, `{"code":10002,"message":"missing token"}`, string(body))

		// 带令牌 ⇒ 200 + {"code":0,"message":"已退出登录"}，**且没有 data 键**
		token, err := auth.GenerateToken(7, "alice", "user")
		require.NoError(t, err)
		code, body = doRequest(t, h, "POST", "/api/v1/user/logout",
			ut.Header{Key: "Authorization", Value: "Bearer " + token})
		require.Equal(t, http.StatusOK, code, "带令牌应回 200：%s", string(body))
		top := assertKeysExact(t, body, "code", "message")
		assert.Equal(t, "0", string(top["code"]))
		assert.Equal(t, `"已退出登录"`, string(top["message"]))
	})

	// ===== 4. GET /api/v1/federation/resolve —— 联邦解析（未启用 ⇒ available:false）=====
	t.Run("federation-resolve", func(t *testing.T) {
		code, body := doRequest(t, h, "GET", "/api/v1/federation/resolve")
		require.Equal(t, http.StatusOK, code, "联邦未启用也应回 200：%s", string(body))

		top := assertKeysExact(t, body, "code", "message", "data", "trace_id")
		assert.Equal(t, "0", string(top["code"]))
		// 同上：resp.Success ⇒ "success"
		assert.Equal(t, `"success"`, string(top["message"]))
		data := jsonKeys(t, top["data"])
		// 未命中时 data 只有 available 一个键（url/node_id/name/expires_at 不出现）
		assert.ElementsMatch(t, []string{"available"}, keysOf(data), "未命中时 data 字段集不符：%s", string(top["data"]))
		assertMatchesContract(t, data, publicapi.FederationResolveData{}, "联邦解析 data")
		assert.Equal(t, "false", string(data["available"]))

		// 带 code 参数（联邦服务未注入 ⇒ 仍 available:false），形状不变
		code, body = doRequest(t, h, "GET", "/api/v1/federation/resolve?code=ABC123")
		require.Equal(t, http.StatusOK, code)
		top = assertKeysExact(t, body, "code", "message", "data", "trace_id")
		data = jsonKeys(t, top["data"])
		assert.ElementsMatch(t, []string{"available"}, keysOf(data), "未命中时 data 字段集不符：%s", string(top["data"]))
		assert.Equal(t, "false", string(data["available"]))
	})

	// ===== 5. GET /api/v1/notifies/public —— 站级公开公告（匿名可访问）=====
	t.Run("notifies-public", func(t *testing.T) {
		g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
		require.NoError(t, err)
		require.NoError(t, g.AutoMigrate(&model.Notify{}))
		sqlDB, err := g.DB()
		require.NoError(t, err)
		sqlDB.SetMaxOpenConns(1)

		svc := notify.NewService(g)
		customHandler.SetNotifyService(svc)
		t.Cleanup(func() { customHandler.SetNotifyService(nil) })

		require.NoError(t, g.Create(&model.Notify{
			Title:   "站级公告",
			Content: "公告正文",
			Type:    "system",
			Level:   "info",
			Status:  1,
		}).Error)

		code, body := doRequest(t, h, "GET", "/api/v1/notifies/public")
		require.Equal(t, http.StatusOK, code, "公开公告应匿名可达 200：%s", string(body))

		top := assertKeysExact(t, body, "code", "message", "data")
		assert.Equal(t, "0", string(top["code"]))
		assert.Equal(t, `"ok"`, string(top["message"]))
		data := jsonKeys(t, top["data"])
		assert.ElementsMatch(t, []string{"items"}, keysOf(data), "公告 data 字段集不符：%s", string(top["data"]))

		var items []map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(data["items"], &items))
		require.Len(t, items, 1, "应有 1 条公告：%s", string(data["items"]))
		// 条目字段集**精确相等**（7 个）；未设生效窗口时 start_at/end_at 显式回 null
		assert.ElementsMatch(t, []string{
			"id", "title", "content", "type", "level", "start_at", "end_at",
		}, keysOf(items[0]), "公告条目字段集不符：%s", string(data["items"]))
		assertMatchesContract(t, items[0], commonmodel.NotifyPublicItem{}, "公告条目")
		assert.Equal(t, `"站级公告"`, string(items[0]["title"]))
		assert.Equal(t, "null", string(items[0]["start_at"]), "未设生效窗口时 start_at 应显式回 null")
		assert.Equal(t, "null", string(items[0]["end_at"]), "未设失效窗口时 end_at 应显式回 null")
	})
}

// keysOf 返回 map 的键集合。
func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// contractField 契约模型的一个 JSON 字段。
type contractField struct {
	name string
	// required=true ⇔ 契约里是 required 字段（thriftgo 生成 json tag 带 ",required"）。
	required bool
}

// contractFields 反射取出契约模型（contracts/gen/<domain>）的 JSON 字段清单。
//
// 这是「**契约 ↔ 线上形状**」的机械连接点：契约里 required 的字段线上一个都不能少，
// 契约里没有的字段线上一个都不能多（多出来 = 契约没治理到的暗字段）。
func contractFields(t *testing.T, model any) []contractField {
	t.Helper()
	typ := reflect.TypeOf(model)
	require.Equal(t, reflect.Struct, typ.Kind(), "契约模型必须是结构体")
	var out []contractField
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.PkgPath != "" { // 非导出字段
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		out = append(out, contractField{name: name, required: strings.Contains(opts, "required")})
	}
	require.NotEmpty(t, out, "契约模型没有解析出任何 JSON 字段")
	return out
}

// assertMatchesContract 断言线上响应体的字段集与契约模型一致：
//   - 契约里 required 的字段**必须**在线上出现（少一个 = 契约没兑现）；
//   - 线上出现的字段**必须**在契约里（多一个 = 契约没治理到的暗字段）。
//
// ⚠️ 契约里的 optional 字段线上可以缺席（omitempty）——缺席不算违规。
func assertMatchesContract(t *testing.T, live map[string]json.RawMessage, model any, what string) {
	t.Helper()
	fields := contractFields(t, model)
	contractNames := map[string]bool{}
	for _, f := range fields {
		contractNames[f.name] = true
		if f.required {
			assert.Contains(t, keysOf(live), f.name,
				"%s：契约里 required 的字段 %q 在线上缺失（契约没兑现）", what, f.name)
		}
	}
	for _, k := range keysOf(live) {
		assert.True(t, contractNames[k],
			"%s：线上字段 %q 不在契约模型里（契约没治理到的暗字段）", what, k)
	}
}

// TestIDLizedRouteResponseShape_ExpireShapePinned 把「expired_at 的 null 语义」
// 单独钉一条：它是本批 IDL 化里**唯一**已知的「契约 optional ↔ 线上 null」差异点
// （契约：optional string 缺省不输出；线上：*time.Time ⇒ null）。
// 本用例记录**线上现状**（null）；若将来实现改为省略字段，本用例会红 ⇒ 提示
// 同步更新契约与前端（前端已用 `?? null` 兜底）。
func TestIDLizedRouteResponseShape_ExpireShapePinned(t *testing.T) {
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}, &model.FileCodeFile{}, &model.PickupCharge{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	svc := share.NewService(g, "http://localhost:12345", nil)
	customHandler.SetShareService(svc)
	t.Cleanup(func() { customHandler.SetShareService(nil) })

	// 显式带过期时间 ⇒ expired_at 是 RFC3339 字符串（非 null）
	expireAt := utils.CalculateExpireTime(1, "day")
	created, err := svc.CreateShare(context.Background(), &share.ShareFileReq{
		Channel:      "direct",
		FilePath:     "",
		Size:         0,
		Text:         "带过期时间的文本",
		ExpiredAt:    expireAt,
		ExpiredCount: utils.CalculateExpireCount("day", 1),
		UploadType:   "anonymous",
	})
	require.NoError(t, err)

	h := idlizedRouterServer()
	code, body := doRequest(t, h, "GET", "/share/metadata/"+created.Code)
	require.Equal(t, http.StatusOK, code, "取件元信息应回 200：%s", string(body))

	top := jsonKeys(t, body)
	data := jsonKeys(t, top["data"])
	require.Contains(t, data, "expired_at")
	assert.NotEqual(t, "null", string(data["expired_at"]), "带过期时间时 expired_at 应是 RFC3339 字符串")
	var got time.Time
	require.NoError(t, json.Unmarshal(data["expired_at"], &got), "expired_at 应可解析为时间：%s", string(data["expired_at"]))
	assert.WithinDuration(t, *expireAt, got, time.Second, "expired_at 应与落库过期时间一致")
}
