// 用户分享列表 status 枚举的 **HTTP 面**守卫（core#149；app 层判据见
// app/share/usershare_status_test.go，这里是「错误必须以 400 + 10001 出网」这一半）。
//
// 负对照：把 app 层校验分支去掉 ⇒ 本用例红（实际会拿到 200 + 未筛选全量）。
package share

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/contracts/errcode"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
)

func TestUserSharesList_UnknownStatus_400(t *testing.T) {
	g := newChargeTestEnv(t, false)
	uid := uint(7)
	require.NoError(t, dao.NewFileCodeRepository(g).Create(context.Background(), &model.FileCode{
		Code: "USTAT01", Text: "普通内容", UserID: &uid, ExpiredCount: -1,
	}))

	code, body := doShareErrReq(t, "GET", "/api/v1/user/shares?status=bogus", "", uid)
	assert.Equal(t, 400, code, "未知 status 应 400（不得 200 返回未筛选全量）：%v", body)
	assert.Equal(t, float64(errcode.CodeInvalidParam), body["code"], "%v", body)
	msg, _ := body["message"].(string)
	assert.Contains(t, msg, "status", "错误消息必须点名参数：%v", body)
}

func TestUserSharesList_ValidStatus_Still200(t *testing.T) {
	g := newChargeTestEnv(t, false)
	uid := uint(7)
	require.NoError(t, dao.NewFileCodeRepository(g).Create(context.Background(), &model.FileCode{
		Code: "USTAT02", Text: "普通内容", UserID: &uid, ExpiredCount: -1,
	}))

	// 合法取值（含空串）必须照旧 200 —— 收紧不得误伤
	for _, status := range []string{"", "all", "active"} {
		path := "/api/v1/user/shares"
		if status != "" {
			path += "?status=" + status
		}
		code, body := doShareErrReq(t, "GET", path, "", uid)
		assert.Equal(t, 200, code, "合法 status=%q 应 200：%v", status, body)
	}
}
