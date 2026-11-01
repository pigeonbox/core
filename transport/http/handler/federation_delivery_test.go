package handler_test

// 跨站投递（联邦前台化，2026-11-01）的端点级断言。
//
// 走**真实生成路由**（gen/router/publicapi）⇒ 路径 / 中间件 / handler 接线
// 一并被覆盖。
//
// 覆盖的判据（与设计稿 §6 验收判据一一对应）：
//  1. 铸槽位：无口令 ⇒ 拒；错口令 ⇒ 拒；对口令 ⇒ 拿到 token 且**响应里没有 6 位码**；
//  2. 投递：未登记的 target_url ⇒ 拒且**一个出站请求都不发**；
//  3. 投递：私网对端且 allow_private=false ⇒ 拒；true ⇒ 放行；
//  4. 投递成功 ⇒ 响应带回**对方柜铸的** pickup_code；
//  5. 转发是**流式**的（对端看到 chunked ⇒ 未整文件缓冲）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/app/request"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/gen/router"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/pigeonbox/core/transport/http/handler"
)

// fakePeer 假对方柜（B 柜）：铸槽位 + 收件。
type fakePeer struct {
	srv *httptest.Server

	slotCalls   int64 // 铸槽位被调次数
	uploadCalls int64 // 收件被调次数
	gotToken    string
	gotBodySize int64
	gotChunked  bool
	pickupCode  string
}

func newFakePeer(t *testing.T, pickupCode string) *fakePeer {
	t.Helper()
	p := &fakePeer{pickupCode: pickupCode}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/federation/delivery-slot", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&p.slotCalls, 1)
		p.gotToken = r.Header.Get("X-Federation-Token")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"token":"slot-token-123"}}`))
	})
	mux.HandleFunc("/api/v1/request/slot-token-123/upload", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&p.uploadCalls, 1)
		for _, te := range r.TransferEncoding {
			if te == "chunked" {
				p.gotChunked = true
			}
		}
		if err := r.ParseMultipartForm(1 << 20); err == nil {
			for _, fhs := range r.MultipartForm.File {
				for _, fh := range fhs {
					p.gotBodySize += fh.Size
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"code":0,"message":"投递成功","data":{"file_count":1,"pickup_code":%q}}`, p.pickupCode)
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

// withDeliveryConf 临时替换全局 conf（对端名单）。
func withDeliveryConf(t *testing.T, peers ...conf.FederationDeliveryPeer) {
	t.Helper()
	old := conf.GetGlobalConfig()
	cfg := &conf.AppConfiguration{}
	if old != nil {
		*cfg = *old
	}
	cfg.Federation.Enabled = true
	cfg.Federation.DeliveryPeers = peers
	conf.SetGlobalConfig(cfg)
	t.Cleanup(func() { conf.SetGlobalConfig(old) })
}

// newDeliveryServer 起一个只注册生成路由的 Hertz + 注入寄件码服务。
func newDeliveryServer(t *testing.T) *server.Hertz {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileRequest{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	handler.SetRequestService(request.NewService(g, nil, nil))

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	router.GeneratedRegister(h)
	return h
}

// multipartBody 造一个 multipart 请求体（字段 + 文件）。
func multipartBody(t *testing.T, fields map[string]string, fileName string, content []byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		require.NoError(t, mw.WriteField(k, v))
	}
	if fileName != "" {
		fw, err := mw.CreateFormFile("files", fileName)
		require.NoError(t, err)
		_, err = fw.Write(content)
		require.NoError(t, err)
	}
	require.NoError(t, mw.Close())
	return buf.Bytes(), mw.FormDataContentType()
}

// perform 打一次请求（multipart 体用 bytes.Reader ⇒ hertz 走 bodyBytes 路径）。
func perform(h *server.Hertz, method, path string, body []byte, contentType string) *ut.ResponseRecorder {
	var b *ut.Body
	if body != nil {
		b = &ut.Body{Body: bytes.NewReader(body), Len: len(body)}
	}
	headers := []ut.Header{}
	if contentType != "" {
		headers = append(headers, ut.Header{Key: "Content-Type", Value: contentType})
	}
	return ut.PerformRequest(h.Engine, method, path, b, headers...)
}

// keysOf 返回 map 的键集合。
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// topKeys 返回响应体的顶层键集合（wire 形状守卫用）。
func topKeys(t *testing.T, body []byte) []string {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &m), "响应体: %s", string(body))
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// decodeData 解响应体的 data 段。
func decodeData(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out struct {
		Code    int            `json:"code"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &out), "响应体: %s", string(body))
	return out.Data
}

// TestFederationDeliverySlot_RequiresSharedToken 铸槽位的口令闸门。
//
// 负对照：把 handler 里的 VerifyDeliveryToken 判断去掉（恒真）⇒
// 「无口令被拒」「错口令被拒」两条断言必红。
func TestFederationDeliverySlot_RequiresSharedToken(t *testing.T) {
	withDeliveryConf(t, conf.FederationDeliveryPeer{URL: "https://b.example.com", Token: "shared-token"})
	h := newDeliveryServer(t)

	// 无口令
	w := ut.PerformRequest(h.Engine, "POST", "/api/v1/federation/delivery-slot", nil)
	assert.Equal(t, http.StatusForbidden, w.Code, "无口令必须被拒")
	assert.Contains(t, w.Body.String(), "共享口令")

	// 错口令
	w = ut.PerformRequest(h.Engine, "POST", "/api/v1/federation/delivery-slot", nil,
		ut.Header{Key: "X-Federation-Token", Value: "wrong"})
	assert.Equal(t, http.StatusForbidden, w.Code, "错口令必须被拒")

	// 正确口令
	w = ut.PerformRequest(h.Engine, "POST", "/api/v1/federation/delivery-slot", nil,
		ut.Header{Key: "X-Federation-Token", Value: "shared-token"})
	require.Equal(t, http.StatusOK, w.Code, "对口令必须放行: %s", w.Body.String())
	assert.ElementsMatch(t, []string{"code", "message", "data", "trace_id"}, topKeys(t, w.Body.Bytes()),
		"槽位响应的顶层键集合固定（契约 code/message/data + 全站 trace_id）")
	data := decodeData(t, w.Body.Bytes())
	assert.ElementsMatch(t, []string{"token"}, keysOf(data), "data 段只允许 token 一个键")
	token, _ := data["token"].(string)
	assert.NotEmpty(t, token, "必须回投递令牌")
	// ⚠️ 此时还没有文件 ⇒ 对方柜**不可能**有 6 位取件码 ⇒ 响应里不许出现它。
	_, hasPickup := data["pickup_code"]
	assert.False(t, hasPickup, "铸槽位阶段不得回 6 位取件码")
}

// TestFederationDeliver_UnregisteredTarget_NoOutboundRequest 未登记的对方柜
// ⇒ 当场拒绝，**一个出站请求都不发**。
//
// 负对照：把 handler 里的 MatchDeliveryPeer 判断去掉（直接用访客给的 URL）
// ⇒ 「对端零请求」这条断言必红。
func TestFederationDeliver_UnregisteredTarget_NoOutboundRequest(t *testing.T) {
	registered := newFakePeer(t, "AB12CD")
	other := newFakePeer(t, "ZZ99ZZ")
	withDeliveryConf(t, conf.FederationDeliveryPeer{URL: registered.srv.URL, Token: "shared-token"})
	h := newDeliveryServer(t)

	body, ct := multipartBody(t, map[string]string{"target_url": other.srv.URL}, "a.txt", []byte("hello"))
	w := perform(h, "POST", "/api/v1/federation/deliver", body, ct)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "不在可投递名单")

	assert.Zero(t, atomic.LoadInt64(&registered.slotCalls), "未命中名单 ⇒ 不得对任何对端发请求")
	assert.Zero(t, atomic.LoadInt64(&registered.uploadCalls), "未命中名单 ⇒ 不得对任何对端发请求")
	assert.Zero(t, atomic.LoadInt64(&other.slotCalls), "未命中名单 ⇒ 不得对访客填的地址发请求")
	assert.Zero(t, atomic.LoadInt64(&other.uploadCalls), "未命中名单 ⇒ 不得对访客填的地址发请求")
}

// TestFederationDeliver_PrivatePeerNeedsAllowPrivate 私网对端：按**对端粒度**
// 放行（不改全局 SSRF 开关）。
//
// 负对照：把 validateDeliveryPeer 里的 allow_private 分支去掉（恒按默认私网策略）
// ⇒ 「allow_private=true 放行」这条断言必红。
func TestFederationDeliver_PrivatePeerNeedsAllowPrivate(t *testing.T) {
	peer := newFakePeer(t, "AB12CD")

	// allow_private=false ⇒ 拒（httptest 的 127.0.0.1 落在私网）
	withDeliveryConf(t, conf.FederationDeliveryPeer{URL: peer.srv.URL, Token: "shared-token"})
	h := newDeliveryServer(t)
	body, ct := multipartBody(t, map[string]string{"target_url": peer.srv.URL}, "a.txt", []byte("hello"))
	w := perform(h, "POST", "/api/v1/federation/deliver", body, ct)
	assert.Equal(t, http.StatusBadRequest, w.Code, "私网对端未放行必须被拒")
	assert.Contains(t, w.Body.String(), "allow_private")
	assert.Zero(t, atomic.LoadInt64(&peer.slotCalls), "被拒时不得发出站请求")

	// allow_private=true ⇒ 放行
	withDeliveryConf(t, conf.FederationDeliveryPeer{URL: peer.srv.URL, Token: "shared-token", AllowPrivate: true})
	body2, ct2 := multipartBody(t, map[string]string{"target_url": peer.srv.URL}, "a.txt", []byte("hello"))
	w2 := perform(h, "POST", "/api/v1/federation/deliver", body2, ct2)
	require.Equal(t, http.StatusOK, w2.Code, "显式 allow_private 必须放行: %s", w2.Body.String())
	assert.Equal(t, int64(1), atomic.LoadInt64(&peer.slotCalls))
}

// TestFederationDeliver_ReturnsPeerPickupCode 投递成功 ⇒ 回传**对方柜铸的**码。
func TestFederationDeliver_ReturnsPeerPickupCode(t *testing.T) {
	peer := newFakePeer(t, "AB12CD")
	withDeliveryConf(t, conf.FederationDeliveryPeer{URL: peer.srv.URL, Token: "shared-token", AllowPrivate: true})
	h := newDeliveryServer(t)

	body, ct := multipartBody(t, map[string]string{"target_url": peer.srv.URL, "title": "给我的"}, "a.txt", []byte("hello"))
	w := perform(h, "POST", "/api/v1/federation/deliver", body, ct)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.ElementsMatch(t, []string{"code", "message", "data", "trace_id"}, topKeys(t, w.Body.Bytes()),
		"投递响应的顶层键集合固定")
	data := decodeData(t, w.Body.Bytes())
	assert.ElementsMatch(t, []string{"file_count", "pickup_code", "target_url"}, keysOf(data),
		"data 段只允许契约声明的三个键")
	assert.Equal(t, "AB12CD", data["pickup_code"], "必须透传对方柜铸的取件码")
	assert.EqualValues(t, 1, data["file_count"])
	assert.Equal(t, peer.srv.URL, data["target_url"])
	assert.Equal(t, int64(1), atomic.LoadInt64(&peer.slotCalls))
	assert.Equal(t, int64(1), atomic.LoadInt64(&peer.uploadCalls))
	assert.Equal(t, "shared-token", peer.gotToken, "铸槽位必须带共享口令")
}

// TestFederationDeliver_StreamsFile 转发是**流式**的：大文件不得整文件缓冲。
//
// 判据 = 对端看到的请求体是 **chunked**（长度未知）⇒ 发送方没有先把文件
// 读进内存再拼 body。
//
// 负对照：把转发实现改成「io.ReadAll 后一次性写出」⇒ Content-Length 变成
// 已知值 ⇒ 对端不再看到 chunked ⇒ 本用例必红。
func TestFederationDeliver_StreamsFile(t *testing.T) {
	peer := newFakePeer(t, "AB12CD")
	withDeliveryConf(t, conf.FederationDeliveryPeer{URL: peer.srv.URL, Token: "shared-token", AllowPrivate: true})
	h := newDeliveryServer(t)

	payload := bytes.Repeat([]byte("A"), 5<<20) // 5MB
	body, ct := multipartBody(t, map[string]string{"target_url": peer.srv.URL}, "big.bin", payload)
	w := perform(h, "POST", "/api/v1/federation/deliver", body, ct)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, peer.gotChunked, "转发请求必须是 chunked（未整文件缓冲）")
	assert.Equal(t, int64(len(payload)), peer.gotBodySize, "文件内容不得丢字节")
}

// TestFederationDeliver_RejectsMissingFile 没有文件 ⇒ 拒（不发请求）。
func TestFederationDeliver_RejectsMissingFile(t *testing.T) {
	peer := newFakePeer(t, "AB12CD")
	withDeliveryConf(t, conf.FederationDeliveryPeer{URL: peer.srv.URL, Token: "shared-token", AllowPrivate: true})
	h := newDeliveryServer(t)

	body, ct := multipartBody(t, map[string]string{"target_url": peer.srv.URL}, "", nil)
	w := perform(h, "POST", "/api/v1/federation/deliver", body, ct)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Zero(t, atomic.LoadInt64(&peer.slotCalls), "没有文件时不得铸槽位")
}

// TestFederationDeliver_MissingTargetURL 缺 target_url ⇒ 拒。
func TestFederationDeliver_MissingTargetURL(t *testing.T) {
	withDeliveryConf(t)
	h := newDeliveryServer(t)
	body, ct := multipartBody(t, map[string]string{}, "a.txt", []byte("x"))
	w := perform(h, "POST", "/api/v1/federation/deliver", body, ct)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "target_url")
}

// TestFederationDeliver_PeerWithoutPickupCode 对方柜没回码 ⇒ 响应里**不带**
// pickup_code 键（optional 语义：缺字段 ≠ 空串）。
func TestFederationDeliver_PeerWithoutPickupCode(t *testing.T) {
	peer := newFakePeer(t, "")
	withDeliveryConf(t, conf.FederationDeliveryPeer{URL: peer.srv.URL, Token: "shared-token", AllowPrivate: true})
	h := newDeliveryServer(t)
	body, ct := multipartBody(t, map[string]string{"target_url": peer.srv.URL}, "a.txt", []byte("x"))
	w := perform(h, "POST", "/api/v1/federation/deliver", body, ct)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	data := decodeData(t, w.Body.Bytes())
	_, has := data["pickup_code"]
	assert.False(t, has, "对方柜没回码时不得回一个空串（前端会显示空白码框）")
}

// TestDeliverySlotIsPublicRoute 铸槽位端点是**公开面**（未被鉴权中间件挡住）：
// 无凭据时到达的是 handler（回 403 业务拒绝），不是 401。
func TestDeliverySlotIsPublicRoute(t *testing.T) {
	withDeliveryConf(t)
	h := newDeliveryServer(t)
	w := ut.PerformRequest(h.Engine, "POST", "/api/v1/federation/delivery-slot", nil)
	assert.NotEqual(t, http.StatusUnauthorized, w.Code,
		"公开端点不得被鉴权中间件挡（应是 handler 内的口令校验 403）")
}
