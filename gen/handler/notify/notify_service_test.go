// notify HTTP 面的**边界**断言（2026-10-25 补零测试面：
// gen/handler/notify 此前 0% 覆盖 ⇒ 「公开 vs 登录专属」与「可选字段」无人守护）。
//
// 该域的三类面：
//
//	· **公开**（Active / ListPublicNotifies）：只出**全站公告**（target_user_id 为空）
//	  ⇒ 定向站内信（取件码/投递详情）绝不能从无认证端点泄露；
//	· **登录专属**（Mine / UnreadCount / MarkRead）：按 user_id 过滤；
//	· **管理面**（/admin/notifies CRUD）：走 AdminMiddleware。
//
// 本组用例钉住三件事：
//
// ① 公开面**不泄露定向通知**（跨用户数据泄露）；
// ② 登录专属面未登录 ⇒ 401（不返回任何他人数据）；
// ③ **可选契约字段缺省不 panic**（optional 指针直接解引用 ⇒ nil deref panic）。
package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	notifyApp "github.com/pigeonbox/core/app/notify"
	"github.com/pigeonbox/core/repo/db/model"
)

// newNotifyHandlerEnv 构造 notify handler 环境（内存 sqlite + 域服务）。
func newNotifyHandlerEnv(t *testing.T) *notifyApp.Service {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.Notify{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	svc := notifyApp.NewService(g)
	SetNotifyService(svc)
	t.Cleanup(func() { SetNotifyService(nil) })
	return svc
}

// doNotifyReq 以指定身份调一个 handler，返回 HTTP 码与响应体。
// userID=0 ⇒ 匿名（不写 user_id ⇒ 模拟「未登录」）。
func doNotifyReq(t *testing.T, method, path, body string, userID uint) (int, map[string]any) {
	t.Helper()
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	withIdentity := func(handler app.HandlerFunc) app.HandlerFunc {
		return func(ctx context.Context, c *app.RequestContext) {
			if userID != 0 {
				c.Set("user_id", userID)
			}
			handler(ctx, c)
		}
	}
	h.GET("/notifies/active", withIdentity(Active))
	h.GET("/api/v1/notifies/mine", withIdentity(Mine))
	h.GET("/api/v1/notifies/unread-count", withIdentity(UnreadCount))
	h.POST("/api/v1/notifies/mark-read", withIdentity(MarkRead))
	h.GET("/admin/notifies", withIdentity(List))

	w := ut.PerformRequest(h.Engine, method, path,
		&ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// TestActive_ExcludesTargeted 公开面**只出全站公告**。
//
// 风险：定向站内信（取件码/投递详情）若从公开端点泄露 ⇒ 匿名访客可枚举他人取件信息。
// 负对照：把 app 层 Active 的 target_user_id 过滤去掉 ⇒ 本用例红。
func TestActive_ExcludesTargeted(t *testing.T) {
	svc := newNotifyHandlerEnv(t)
	ctx := context.Background()

	_, err := svc.Create(ctx, notifyApp.CreateReq{
		Title: "全站公告", Content: "公开内容", Type: "maintenance", Level: "info",
	})
	require.NoError(t, err)
	_, err = svc.CreateForUser(ctx, 2, "取件通知", "你的取件码 123456", "pickup", "info")
	require.NoError(t, err)

	code, body := doNotifyReq(t, "GET", "/notifies/active", "", 0)
	require.Equal(t, http.StatusOK, code, "公开公告面应 200：%v", body)

	items, _ := body["data"].(map[string]any)
	raw, _ := items["items"].([]any)
	require.Len(t, raw, 1, "公开面只应返回 1 条全站公告（定向的那条必须不可见）：%v", body)
	first, _ := raw[0].(map[string]any)
	assert.Equal(t, "全站公告", first["title"])
}

// TestMine_RequiresLogin 未登录访问「我的通知」⇒ 401。
//
// 负对照：把 requireUserID 的 401 分支去掉 ⇒ 本用例红。
func TestMine_RequiresLogin(t *testing.T) {
	newNotifyHandlerEnv(t)

	code, body := doNotifyReq(t, "GET", "/api/v1/notifies/mine", "", 0)
	assert.Equal(t, http.StatusUnauthorized, code,
		"未登录访问我的通知应 401（不得返回任何他人定向通知）：%v", body)
}

// TestMine_OnlyOwnAndBroadcast 我的通知**只含广播 + 定向给我**的。
//
// 负对照：把 ListForUser 的 target_user_id 过滤去掉 ⇒ 本用例红。
func TestMine_OnlyOwnAndBroadcast(t *testing.T) {
	svc := newNotifyHandlerEnv(t)
	ctx := context.Background()

	_, err := svc.Create(ctx, notifyApp.CreateReq{
		Title: "全站公告", Content: "公开内容", Type: "maintenance", Level: "info",
	})
	require.NoError(t, err)
	_, err = svc.CreateForUser(ctx, 1, "给1号", "1号的内容", "pickup", "info")
	require.NoError(t, err)
	_, err = svc.CreateForUser(ctx, 2, "给2号", "2号的内容", "pickup", "info")
	require.NoError(t, err)

	code, body := doNotifyReq(t, "GET", "/api/v1/notifies/mine", "", 1)
	require.Equal(t, http.StatusOK, code, "登录用户应 200：%v", body)

	data, _ := body["data"].(map[string]any)
	raw, _ := data["items"].([]any)
	// 1 条广播 + 1 条定向给 1 号；**不得**含定向给 2 号的那条。
	require.Len(t, raw, 2, "1号只应看到广播 + 自己的定向通知：%v", body)
	for _, it := range raw {
		m, _ := it.(map[string]any)
		assert.NotEqual(t, "给2号", m["title"], "不得看到别人的定向通知：%v", body)
	}
}

// TestMarkRead_RequiresLogin 未登录标记已读 ⇒ 401。
func TestMarkRead_RequiresLogin(t *testing.T) {
	newNotifyHandlerEnv(t)

	code, body := doNotifyReq(t, "POST", "/api/v1/notifies/mark-read", `{"all":true}`, 0)
	assert.Equal(t, http.StatusUnauthorized, code,
		"未登录标记已读应 401：%v", body)
}

// TestMarkRead_RequiresAllTrue 只支持 all=true（历史语义：body 可为空 ⇒ 400）。
func TestMarkRead_RequiresAllTrue(t *testing.T) {
	newNotifyHandlerEnv(t)

	code, body := doNotifyReq(t, "POST", "/api/v1/notifies/mark-read", `{"all":false}`, 1)
	assert.Equal(t, http.StatusBadRequest, code,
		"all!=true 应 400（不得静默标为全部已读）：%v", body)
}

// TestList_MissingOptionalPageDoesNotPanic 管理面列表**不传分页参数**不得 panic。
//
// ⚠️ 这是真实缺陷的复现：`ListReq.Page/PageSize` 是 thrift **optional** 指针
// （`*int32`），而 handler 直接 `int(*req.Page)` 解引用 ⇒ 客户端不带 page 参数时
// **nil pointer dereference panic** ⇒ 整个管理台「公告管理」页 500。
//
// 负对照：把 handler 改回 `int(*req.Page)` 直解引用 ⇒ 本用例红（panic）。
func TestList_MissingOptionalPageDoesNotPanic(t *testing.T) {
	newNotifyHandlerEnv(t)

	// 不带任何 query 参数 ⇒ optional page/page_size 为 nil。
	code, body := doNotifyReq(t, "GET", "/admin/notifies", "", 1)
	assert.Equal(t, http.StatusOK, code,
		"不带分页参数时管理面列表应正常返回（不得 panic/500）：%v", body)
	assert.NotEqual(t, float64(500), body["code"],
		"业务码不应是 500（说明服务端 panic 后被兜底成 500）：%v", body)
}
