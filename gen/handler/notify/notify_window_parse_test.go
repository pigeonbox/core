// 公告生效窗口参数的**严格解析**守卫（core#146，与 #102/#104/#110/#113/#123 同族）。
//
// 缺陷形态：Create/Update 对 start_at/end_at 做 time.Parse 且 **err == nil 才用**
// ⇒ 非法时间被静默丢弃：
//
//	· Create：本该「定时生效/到期」的公告落库时**没有窗口** ⇒ 立刻全站可见且永不过期
//	  （Active 的过滤是 `start_at IS NULL OR start_at <= now`）；
//	· Update：管理员改窗口**静默 no-op**，接口仍回 success ⇒ 回读才发现纹丝不动。
//
// 判据：非法时间 ⇒ 400 + 业务码 10001 + 点名参数与期望格式（全站信封）。
// 负对照：把 handler 的错误分支改回「忽略 err」⇒ 本文件全部转红。
package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/contracts/errcode"
	notifyApp "github.com/pigeonbox/core/app/notify"
)

// listAllReq 取全量列表请求（page/page_size 走 Policy 归一化）。
func listAllReq() notifyApp.ListReq {
	return notifyApp.ListReq{Page: 1, PageSize: 20}
}

// doNotifyWindowReq 打一次公告管理面请求（Create / Update）。
func doNotifyWindowReq(t *testing.T, method, path, body string) (int, map[string]any) {
	t.Helper()
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	withIdentity := func(handler app.HandlerFunc) app.HandlerFunc {
		return func(ctx context.Context, c *app.RequestContext) {
			c.Set("user_id", uint(1))
			handler(ctx, c)
		}
	}
	h.POST("/admin/notifies", withIdentity(Create))
	h.PUT("/admin/notifies/:id", withIdentity(Update))

	w := ut.PerformRequest(h.Engine, method, path,
		&ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// notifyWindowCreateBody 造一条带窗口的创建请求。
func notifyWindowCreateBody(startAt, endAt string) string {
	parts := []string{`"title":"窗口守卫"`, `"content":"内容"`, `"type":"system"`, `"level":"info"`}
	if startAt != "" {
		parts = append(parts, `"start_at":"`+startAt+`"`)
	}
	if endAt != "" {
		parts = append(parts, `"end_at":"`+endAt+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// TestCreate_InvalidStartAt_Rejected 非法 start_at ⇒ 400 + 10001 + 点名参数。
//
// 负对照：Create 的 `if err == nil { startAt = &t }` 改回不报错 ⇒ 本用例红
// （实际会拿到 200）。
func TestCreate_InvalidStartAt_Rejected(t *testing.T) {
	newNotifyHandlerEnv(t)

	code, body := doNotifyWindowReq(t, "POST", "/admin/notifies",
		notifyWindowCreateBody("not-a-time", ""))
	assert.Equal(t, http.StatusBadRequest, code,
		"非法 start_at 应 400（不得静默丢窗口）：%v", body)
	assert.Equal(t, float64(errcode.CodeInvalidParam), body["code"], "%v", body)
	msg, _ := body["message"].(string)
	assert.Contains(t, msg, "start_at", "错误消息必须点名参数：%v", body)
}

// TestCreate_InvalidEndAt_Rejected 非法 end_at 同口径。
func TestCreate_InvalidEndAt_Rejected(t *testing.T) {
	newNotifyHandlerEnv(t)

	code, body := doNotifyWindowReq(t, "POST", "/admin/notifies",
		notifyWindowCreateBody("", "2026-13-45T99:99:99Z"))
	assert.Equal(t, http.StatusBadRequest, code,
		"非法 end_at 应 400：%v", body)
	assert.Equal(t, float64(errcode.CodeInvalidParam), body["code"], "%v", body)
	msg, _ := body["message"].(string)
	assert.Contains(t, msg, "end_at", "错误消息必须点名参数：%v", body)
}

// TestCreate_ValidWindow_Persisted 合法 RFC3339 窗口照常落库（收紧不得误伤）。
func TestCreate_ValidWindow_Persisted(t *testing.T) {
	svc := newNotifyHandlerEnv(t)

	code, body := doNotifyWindowReq(t, "POST", "/admin/notifies",
		notifyWindowCreateBody("2030-01-01T00:00:00Z", "2030-01-02T00:00:00Z"))
	require.Equal(t, http.StatusOK, code, "合法窗口应 200：%v", body)

	items, err := svc.List(context.Background(), listAllReq())
	require.NoError(t, err)
	require.Len(t, items.Items, 1)
	require.NotNil(t, items.Items[0].StartAt, "合法 start_at 必须落库")
	assert.Equal(t, "2030-01-01T00:00:00Z", items.Items[0].StartAt.UTC().Format("2006-01-02T15:04:05Z07:00"))
	require.NotNil(t, items.Items[0].EndAt, "合法 end_at 必须落库")
}

// TestUpdate_InvalidEndAt_Rejected 非法 end_at ⇒ 400（不得 200 静默 no-op）。
//
// 负对照：Update 的解析错误分支改回忽略 ⇒ 本用例红（拿到 200）。
func TestUpdate_InvalidEndAt_Rejected(t *testing.T) {
	svc := newNotifyHandlerEnv(t)
	// 先建一条有窗口的公告，确保 update 走到解析段。
	createCode, createBody := doNotifyWindowReq(t, "POST", "/admin/notifies",
		notifyWindowCreateBody("2030-01-01T00:00:00Z", ""))
	require.Equal(t, http.StatusOK, createCode, "前置创建应 200：%v", createBody)

	items, lerr := svc.List(context.Background(), listAllReq())
	require.NoError(t, lerr)
	require.Len(t, items.Items, 1)
	id := items.Items[0].ID

	code, body := doNotifyWindowReq(t, "PUT", "/admin/notifies/"+strconv.FormatUint(uint64(id), 10),
		`{"end_at":"yesterday-ish"}`)
	assert.Equal(t, http.StatusBadRequest, code,
		"非法 end_at 的更新应 400（不得 200 静默不改）：%v", body)
	assert.Equal(t, float64(errcode.CodeInvalidParam), body["code"], "%v", body)
	msg, _ := body["message"].(string)
	assert.Contains(t, msg, "end_at", "错误消息必须点名参数：%v", body)

	// 窗口必须仍是原值（没有被清掉/改掉）
	after, aerr := svc.List(context.Background(), listAllReq())
	require.NoError(t, aerr)
	require.Len(t, after.Items, 1)
	assert.Nil(t, after.Items[0].EndAt, "非法 end_at 不得改动既有窗口")
}
