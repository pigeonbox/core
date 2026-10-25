// presign handler 层的**错误映射**与**幂等**断言（2026-10-25 补零测试面：
// gen/handler/presign 此前 0% 覆盖 ⇒ 错误码→HTTP 状态的映射无人守护）。
//
// 缺陷（审计 A1 同族）：3xxxx 族业务码在 `pkg/resp.httpStatusForCode` **整族
// 未枚举** ⇒ 一律落 `default` ⇒ **HTTP 200**。其中最要紧的一条：
// 「上传会话已完成 ⇒ 拒绝重放直传」是**安全相关的拒绝**，却
//
//	① HTTP 200（客户端按状态分支 ⇒ 当成成功）；
//	② 与「预签名生成失败」共用 30008 ⇒ 消息也是错的（「预签名生成失败」）。
//
// ⇒ 已修：独立业务码 30014 + 409 Conflict；3xxxx 族按语义补齐 HTTP 状态。
//
// 断言必须能变红（负对照）：把 30014/30009 的映射去掉 ⇒ 对应用例红。
package presign

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/contracts/errcode"
	presignapp "github.com/pigeonbox/core/app/presign"
)

// stubShareGateway 桩：CreateShare 记下 upload_id → 分享码；FindShareByUploadID
// 可配置为「查不到」（模拟历史遗留的「meta 标完成但分享已不在」）。
type stubShareGateway struct {
	byUploadID map[string]string
	// forgetAfter >0 ⇒ 第 N 次回读起返回「查不到」（模拟分享已不在）
	forgetAfter int
	lookups     int
}

func (g *stubShareGateway) CreateShare(_ context.Context, req *presignapp.ShareCreateReq) (*presignapp.ShareCreateResp, error) {
	if g.byUploadID == nil {
		g.byUploadID = map[string]string{}
	}
	g.byUploadID[req.UploadID] = "SHARE01"
	return &presignapp.ShareCreateResp{Code: "SHARE01", PickupCode: "PICK01"}, nil
}

func (g *stubShareGateway) FindShareByUploadID(_ context.Context, uploadID string) (string, error) {
	g.lookups++
	if g.forgetAfter > 0 && g.lookups >= g.forgetAfter {
		return "", nil
	}
	if g.byUploadID == nil {
		return "", nil
	}
	return g.byUploadID[uploadID], nil
}

// newHandlerEnv 构造 presign handler 环境（内存 KV + 桩 share 网关）。
func newHandlerEnv(t *testing.T, gw presignapp.ShareServiceInterface) *presignapp.Service {
	t.Helper()
	svc := presignapp.NewService(nil, nil, "http://test.local", "test-signing-key-0123456789")
	SetService(svc)
	if gw != nil {
		SetShareService(gw)
	}
	t.Cleanup(func() {
		SetService(nil)
		SetShareService(nil)
	})
	return svc
}

// postComplete 以 JSON 调 Complete，返回 HTTP 码与响应体。
func postComplete(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.POST("/api/v1/presign/complete", Complete)
	w := ut.PerformRequest(h.Engine, "POST", "/api/v1/presign/complete",
		&ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// initSession 走一次 Init，拿到真实的 upload_id 与 token。
// ⚠️ Init **自己生成** upload_id（覆盖入参 ⇒ 客户端不可控）⇒ 必须用返回值。
func initSession(t *testing.T, svc *presignapp.Service) (uploadID, token string) {
	t.Helper()
	res, err := svc.Init(context.Background(), presignapp.InitMeta{
		FileName: "a.bin", FileSize: 10, ExpireValue: 1, ExpireStyle: "day",
	})
	require.NoError(t, err)
	require.NotEmpty(t, res.UploadID)
	require.NotEmpty(t, res.Token)
	return res.UploadID, res.Token
}

// TestComplete_SecondCall_ReusesSameShare 同一 upload_id 二次 Complete ⇒
// **幂等返回同一个分享码**（core v0.32.6 的上传完成面幂等修复在 HTTP 层的
// 行为断言）——**不新建分享** ⇒ owner 的「取件 N 次」额度不翻倍。
func TestComplete_SecondCall_ReusesSameShare(t *testing.T) {
	gw := &stubShareGateway{}
	svc := newHandlerEnv(t, gw)
	uploadID, token := initSession(t, svc)

	code, body := postComplete(t, `{"upload_id":"`+uploadID+`","token":"`+token+`"}`)
	require.Equal(t, http.StatusOK, code, "首次 Complete 应成功：%v", body)
	firstCode, _ := body["data"].(map[string]any)["code"].(string)
	require.NotEmpty(t, firstCode)

	code, body = postComplete(t, `{"upload_id":"`+uploadID+`","token":"`+token+`"}`)
	require.Equal(t, http.StatusOK, code, "二次 Complete 应幂等成功：%v", body)
	secondCode, _ := body["data"].(map[string]any)["code"].(string)
	assert.Equal(t, firstCode, secondCode, "二次 Complete 必须复用同一个分享码（不得新建）")
}

// TestComplete_AlreadyCompletedNoShare_Conflict409 历史遗留态：meta 标了
// 「已完成」但**分享已查不到** ⇒ 拒绝重放 ⇒ **409 冲突** + 独立业务码
// 30014（修前：HTTP 200 + 30008「预签名生成失败」—— 状态与消息都错）。
func TestComplete_AlreadyCompletedNoShare_Conflict409(t *testing.T) {
	// forgetAfter=1 ⇒ 第二次 Complete 时回读「查不到」⇒ 命中 ErrAlreadyComplete
	gw := &stubShareGateway{forgetAfter: 1}
	svc := newHandlerEnv(t, gw)
	uploadID, token := initSession(t, svc)

	// 第一次：建分享成功
	code, body := postComplete(t, `{"upload_id":"`+uploadID+`","token":"`+token+`"}`)
	require.Equal(t, http.StatusOK, code, "首次 Complete 应成功：%v", body)

	// 第二次：分享查不到 ⇒ 409
	code, body = postComplete(t, `{"upload_id":"`+uploadID+`","token":"`+token+`"}`)
	assert.Equal(t, http.StatusConflict, code,
		"会话已完成（分享已不在）拒绝重放应是 409 Conflict（修前 200：客户端按状态分支会当成成功）：%v", body)
	assert.Equal(t, float64(errcode.CodePresignCompleted), body["code"],
		"业务码应是 30014「上传会话已完成」（不得复用 30008「预签名生成失败」）")
}

// TestComplete_InvalidToken_Forbidden 令牌无效 ⇒ **403**（刻意不用 401：
// 前端把 401 当会话失效 ⇒ 触发 refresh/跳登录，而 presign 令牌与会话无关）。
func TestComplete_InvalidToken_Forbidden(t *testing.T) {
	svc := newHandlerEnv(t, &stubShareGateway{})
	uploadID, _ := initSession(t, svc)

	code, body := postComplete(t, `{"upload_id":"`+uploadID+`","token":"wrong-token"}`)
	assert.Equal(t, http.StatusForbidden, code,
		"令牌无效应是 403（用 401 会让前端误判为会话失效）：%v", body)
	assert.Equal(t, float64(errcode.CodePresignToken), body["code"])
}
