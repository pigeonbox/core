// 寄件码（反向收件）HTTP 面的**边界**断言（2026-10-25 补零测试面：
// gen/handler/request 此前 0% 覆盖 ⇒ 「登录专属 vs 公开」的边界无人守护）。
//
// 该域的两类面：
//
//	· **登录专属**（Create / ListMine / Delete）：必须有 user_id，否则 401；
//	· **公开**（PublicGet / GuestUpload）：凭 token 访问（token 即能力凭证）。
//
// 越权是这类面的头号风险：Delete 若不校验归属 ⇒ 任何登录用户可删别人的
// 投递链接。本组用例把归属校验钉住。
//
// 断言必须能变红（负对照）：把 Delete 的归属校验去掉 ⇒ 用例红。
package request

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

	requestApp "github.com/pigeonbox/core/app/request"
	"github.com/pigeonbox/core/repo/db/model"
)

// newRequestHandlerEnv 构造 request handler 环境（内存 sqlite + 域服务）。
func newRequestHandlerEnv(t *testing.T) *requestApp.Service {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileRequest{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	svc := requestApp.NewService(g, nil, nil)
	SetRequestService(svc)
	t.Cleanup(func() { SetRequestService(nil) })
	return svc
}

// doReq 以指定身份调一个 handler，返回 HTTP 码与响应体。
// userID=0 ⇒ 匿名（不写 user_id ⇒ 模拟「未登录」）。
func doReq(t *testing.T, method, path, body string, userID uint) (int, map[string]any) {
	t.Helper()
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	// 身份注入：auth 中间件在生产里写 c.Set("user_id", …) ⇒ 测试里同样注入
	withIdentity := func(handler app.HandlerFunc) app.HandlerFunc {
		return func(ctx context.Context, c *app.RequestContext) {
			if userID != 0 {
				c.Set("user_id", userID)
			}
			handler(ctx, c)
		}
	}
	h.DELETE("/api/v1/user/requests/:token", withIdentity(Delete))
	h.GET("/api/v1/user/requests", withIdentity(ListMine))
	h.GET("/request/:token", withIdentity(PublicGet))

	w := ut.PerformRequest(h.Engine, method, path,
		&ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// TestListMine_RequiresLogin 未登录 ⇒ 401（不泄露任何投递链接信息）。
func TestListMine_RequiresLogin(t *testing.T) {
	newRequestHandlerEnv(t)

	code, body := doReq(t, "GET", "/api/v1/user/requests", "", 0)
	assert.Equal(t, http.StatusUnauthorized, code, "未登录访问我的投递列表应 401：%v", body)
}

// TestDelete_RejectsNonOwner 删除**别人**的投递链接必须失败（归属校验）。
func TestDelete_RejectsNonOwner(t *testing.T) {
	svc := newRequestHandlerEnv(t)
	ctx := context.Background()

	// 造一个属于 user 1 的投递链接
	created, err := svc.Create(ctx, 1, requestApp.CreateReq{
		Title: "收件", MaxFiles: 3, MaxBytes: 1024, ExpireValue: 1, ExpireStyle: "day",
	})
	require.NoError(t, err)
	require.NotEmpty(t, created.Token)

	// user 2 来删 ⇒ 不得成功
	code, body := doReq(t, "DELETE", "/api/v1/user/requests/"+created.Token, "", 2)
	assert.NotEqual(t, http.StatusOK, code,
		"非归属用户删除投递链接必须被拒（否则任何登录用户可删别人的投递链接）：%v", body)
}
