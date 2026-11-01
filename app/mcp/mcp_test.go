package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	adminApp "github.com/pigeonbox/core/app/admin"
	shareApp "github.com/pigeonbox/core/app/share"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/pigeonbox/core/storage"
)

type nopStorage struct{}

func (nopStorage) SaveFile(_ context.Context, _ *multipart.FileHeader, _ string) (*storage.FileOperationResult, error) {
	return &storage.FileOperationResult{Success: true}, nil
}
func (nopStorage) DeleteFile(_ context.Context, _ string) error        { return nil }
func (nopStorage) GetFile(_ context.Context, _ string) ([]byte, error) { return nil, nil }
func (nopStorage) FileExists(_ context.Context, _ string) bool         { return true }
func (nopStorage) SaveStream(_ context.Context, _ string, _ io.Reader, _ int64) (int64, error) {
	return 0, nil
}
func (nopStorage) SaveChunk(_ context.Context, _ string, _ int, _ []byte) error   { return nil }
func (nopStorage) MergeChunks(_ context.Context, _ string, _ int, _ string) error { return nil }
func (nopStorage) CleanChunks(_ context.Context, _ string) error                  { return nil }
func (nopStorage) GetFileSize(_ context.Context, _ string) (int64, error)         { return 0, nil }
func (nopStorage) GetFileURL(_ context.Context, _ string) (string, error)         { return "", nil }
func (nopStorage) GetFileReader(_ context.Context, _ string) (io.ReadCloser, int64, error) {
	return nil, 0, nil
}

// shareAPIAdapter 测试桥：mcp.ShareAPI 接真实 share.Service
// （生产由 bootstrap 的 mcpShareAdapter 注入同形适配器；测试文件允许跨层 import）。
type shareAPIAdapter struct{ svc *shareApp.Service }

func (a shareAPIAdapter) CreateTextShare(ctx context.Context, text string, expireValue int, expireStyle string,
	requireAuth bool, passwordHash string, userID *uint, ownerIP, customCode string) (ShareCreated, error) {
	resp, err := a.svc.ShareTextWithAuth(ctx, text, expireValue, expireStyle, requireAuth, passwordHash, userID, ownerIP, false, customCode, "")
	if err != nil {
		return ShareCreated{}, err
	}
	return ShareCreated{Code: resp.Code, PickupCode: resp.PickupCode, FullShareURL: resp.FullShareURL}, nil
}

func (a shareAPIAdapter) ShareBytes(ctx context.Context, opts FileShareOpts) (ShareCreated, error) {
	resp, err := a.svc.ShareBytes(ctx, shareApp.ShareBytesOpts{
		FileName: opts.FileName, Content: opts.Content,
		ExpireValue: opts.ExpireValue, ExpireStyle: opts.ExpireStyle,
		PasswordHash: opts.PasswordHash, CustomCode: opts.CustomCode,
		UserID: opts.UserID, OwnerIP: opts.OwnerIP,
	})
	if err != nil {
		return ShareCreated{}, err
	}
	return ShareCreated{Code: resp.Code, PickupCode: resp.PickupCode, FullShareURL: resp.FullShareURL}, nil
}

func (a shareAPIAdapter) ShareFiles(ctx context.Context, code string) ([]ShareFileInfo, error) {
	items, err := a.svc.ListShareFiles(ctx, code)
	if err != nil {
		return nil, err
	}
	out := make([]ShareFileInfo, len(items))
	for i, it := range items {
		out[i] = ShareFileInfo{Name: it.Name, Size: it.Size}
	}
	return out, nil
}

// adminAPIAdapter 测试桥：mcp.AdminAPI 接真实 admin.Service
// （生产由 bootstrap 的 mcpAdminAdapter 注入同形适配器）。
type adminAPIAdapter struct{ svc *adminApp.Service }

func (a adminAPIAdapter) DeleteShareByID(ctx context.Context, id uint) error {
	return a.svc.DeleteFile(ctx, id)
}

func (a adminAPIAdapter) SystemStats(ctx context.Context) (*SystemStats, error) {
	st, err := a.svc.GetStats(ctx)
	if err != nil {
		return nil, err
	}
	return &SystemStats{
		TotalFiles: st.TotalFiles, TotalUsers: st.TotalUsers, TotalSize: st.TotalSize,
		TodayUploads: st.TodayUploads, ExpiredFiles: st.ExpiredFiles,
	}, nil
}

func (a adminAPIAdapter) StorageStatus(ctx context.Context) (*StorageStatusInfo, error) {
	st, err := a.svc.GetStorageStatus(ctx)
	if err != nil {
		return nil, err
	}
	return &StorageStatusInfo{
		StorageType: st.StorageType, TotalSpace: st.TotalSpace, UsedSpace: st.UsedSpace,
		UsagePercent: st.UsagePercent, FileCount: st.FileCount,
	}, nil
}

func (a adminAPIAdapter) Users(ctx context.Context, page, pageSize int) ([]UserRow, int64, error) {
	users, total, err := a.svc.GetUsers(ctx, page, pageSize)
	if err != nil {
		return nil, 0, err
	}
	rows := make([]UserRow, len(users))
	for i, u := range users {
		rows[i] = UserRow{ID: u.ID, Username: u.Username, Email: u.Email, Status: u.Status}
	}
	return rows, total, nil
}

func (a adminAPIAdapter) CleanExpired(ctx context.Context) (int64, int64, error) {
	return a.svc.CleanExpiredFiles(ctx)
}

// newMCPTestDB 构造注入用内存 sqlite（迁移 MCP 工具用表）。
func newMCPTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}, &model.FileCodeFile{}, &model.User{}, &model.AdminOperationLog{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return g
}

// newMCPTestServiceWithDB 同 newMCPTestService，额外返回测试库句柄（测试直查记录用）。
func newMCPTestServiceWithDB(t *testing.T) (*Service, *gorm.DB) {
	t.Helper()
	g := newMCPTestDB(t)
	svc := NewService(g, "test-1.0.0")
	// share 服务与下载工具共用同一存储桩（写入/读取同 map 才能对上路径）
	ms := mapStorage{data: map[string][]byte{}}
	svc.SetShareService(shareAPIAdapter{shareApp.NewService(g, "http://test.local", ms)})
	svc.SetAdminService(adminAPIAdapter{adminApp.NewService(g)})
	svc.SetStorageService(ms)
	return svc, g
}

// newMCPTestService 构造注入 mock 依赖的 MCP service。
func newMCPTestService(t *testing.T) *Service {
	t.Helper()
	svc, _ := newMCPTestServiceWithDB(t)
	return svc
}

// rpc 发送 JSON-RPC 请求并解析响应（默认管理员身份）
func rpc(t *testing.T, s *Service, method string, params any) (status int, out map[string]any) {
	t.Helper()
	return rpcAs(t, s, Caller{Admin: true, UserID: 1, IP: "127.0.0.1"}, method, params)
}

// rpcAs 以指定调用者身份发送 JSON-RPC 请求并解析响应
func rpcAs(t *testing.T, s *Service, caller Caller, method string, params any) (status int, out map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	st, raw := postRPC(t, s, caller, body)
	require.NotEmpty(t, raw, "%s 应返回响应体", method)
	require.NoError(t, json.Unmarshal(raw, &out))
	return st, out
}

// postRPC 以 HTTP 形态投递 JSON-RPC 报文（官方 SDK 的 Streamable HTTP 传输：
// Content-Type 须 application/json，Accept 须同时含 json 与 text/event-stream
// ——缺任一项 SDK 直接回 400/415）。调用者身份随 ctx 注入（与生产 transport
// 层同路径）。
func postRPC(t *testing.T, s *Service, caller Caller, body []byte) (int, []byte) {
	t.Helper()
	return postRPCVer(t, s, caller, body, "")
}

// postRPCVer 携带 MCP-Protocol-Version 头投递报文（2026-07-28 新协议逐请求
// 版本协商，SEP-2575：server/discover 等新方法要求该头）。
func postRPCVer(t *testing.T, s *Service, caller Caller, body []byte, ver string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if ver != "" {
		// 新协议（SEP-2575）：方法名与调用名走头部，且须与 body 一致
		req.Header.Set("Mcp-Protocol-Version", ver)
		var probe struct {
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &probe)
		if probe.Method != "" {
			req.Header.Set("Mcp-Method", probe.Method)
		}
		if probe.Params.Name != "" {
			req.Header.Set("Mcp-Name", probe.Params.Name)
		}
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req.WithContext(WithCaller(req.Context(), caller)))
	return rec.Code, rec.Body.Bytes()
}

// toolOut 取 tools/call 的结构化出参（structuredContent；工具层 2026-10-22
// 起为类型化出参，agent 不必再从散文里正则刮字段）。
func toolOut(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	result, ok := out["result"].(map[string]any)
	require.True(t, ok, "应返回 result: %v", out)
	sc, ok := result["structuredContent"].(map[string]any)
	require.True(t, ok, "应返回 structuredContent: %v", result)
	return sc
}

// toolIsErr 工具执行错误判定（isError=true）。
func toolIsErr(out map[string]any) bool {
	result, _ := out["result"].(map[string]any)
	isErr, _ := result["isError"].(bool)
	return isErr
}

// toolErrText 工具执行错误文本（isError 时 content 的文本块）。
func toolErrText(t *testing.T, out map[string]any) string {
	t.Helper()
	result, _ := out["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	blk, _ := content[0].(map[string]any)
	text, _ := blk["text"].(string)
	return text
}

func TestInitialize(t *testing.T) {
	s := newMCPTestService(t)
	// 握手期旧版（历史客户端口径）：服务端回协商同一版本，老客户端零改造
	st, out := rpc(t, s, "initialize", map[string]any{
		"protocolVersion": "2025-03-26",
		"clientInfo":      map[string]any{"name": "test-client", "version": "0"},
	})
	assert.Equal(t, 200, st)
	result := out["result"].(map[string]any)
	assert.Equal(t, "2025-03-26", result["protocolVersion"], "老客户端协议版本应原样协商")
	assert.Equal(t, serverName, result["serverInfo"].(map[string]any)["name"])
	assert.Contains(t, result["capabilities"].(map[string]any), "tools")
}

// TestServerDiscover 最新规范（2026-07-28）的发现方法：一次拿到服务端支持的
// 协议版本全集 + 能力（旧协议用 initialize 握手，新协议改用 server/discover，
// 逐请求版本协商须带 MCP-Protocol-Version 头）。
func TestServerDiscover(t *testing.T) {
	s := newMCPTestService(t)
	// 新协议逐请求版本协商（SEP-2575）：头部 + _meta 三元组须一致
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "server/discover",
		"params": map[string]any{"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		}},
	})
	st, raw := postRPCVer(t, s, Caller{Admin: true}, body, "2026-07-28")
	assert.Equal(t, 200, st)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	result, ok := out["result"].(map[string]any)
	require.True(t, ok, "应返回 result: %s", string(raw))
	versions, _ := result["supportedVersions"].([]any)
	assert.Contains(t, versions, "2026-07-28", "应支持最新规范版本")
	assert.Contains(t, versions, "2025-03-26", "应兼容历史客户端协议版本")
	assert.Contains(t, result["capabilities"].(map[string]any), "tools")
}

func TestNotification_NoResponse(t *testing.T) {
	s := newMCPTestService(t)
	body := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	st, raw := postRPC(t, s, Caller{Admin: true}, body)
	assert.Equal(t, 202, st)
	assert.Empty(t, raw, "通知不返回响应体")
}

func TestToolsList_AllTools(t *testing.T) {
	s := newMCPTestService(t)
	_, out := rpc(t, s, "tools/list", nil)
	tools := out["result"].(map[string]any)["tools"].([]any)
	assert.Len(t, tools, 18)
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"share_text", "share_file", "create_request_code", "preview_share",
		"get_share", "get_share_content",
		"download_share_file", "list_shares", "delete_share",
		"get_system_status", "get_storage_info", "list_users", "cleanup_expired",
		"federation_status", "federation_resolve",
		"p2p_peers", "p2p_invites", "p2p_accept"} {
		assert.True(t, names[want], "缺少工具 %s", want)
	}
	// Wave B'：直传四工具随 Box 侧直传编排层整体退役（直传由 PigeonBeam 承担）
	for _, gone := range []string{"p2p_status", "p2p_tasks", "p2p_send", "p2p_recv"} {
		assert.False(t, names[gone], "直传工具 %s 应已退役", gone)
	}
}

// TestToolsList_InferredSchema 工具 schema 由官方 SDK 从类型化入参推断
// （JSON Schema 2020-12）：属性名/类型/必填项齐备，模型可直接照此填参。
func TestToolsList_InferredSchema(t *testing.T) {
	s := newMCPTestService(t)
	_, out := rpc(t, s, "tools/list", nil)
	tools := out["result"].(map[string]any)["tools"].([]any)
	var shareText map[string]any
	for _, tl := range tools {
		m := tl.(map[string]any)
		if m["name"] == "share_text" {
			shareText = m
		}
	}
	require.NotNil(t, shareText, "应含 share_text")
	schema, ok := shareText["inputSchema"].(map[string]any)
	require.True(t, ok, "share_text 应带 inputSchema: %v", shareText)
	assert.Equal(t, "object", schema["type"])
	props, ok := schema["properties"].(map[string]any)
	require.True(t, ok, "inputSchema 应有 properties: %v", schema)
	assert.Contains(t, props, "text")
	assert.Contains(t, props, "expire_value")
	assert.Contains(t, props, "expire_style")
	// text 必填、expire_value 可选（omitempty）
	required, _ := schema["required"].([]any)
	assert.Contains(t, required, "text")
	assert.NotContains(t, required, "expire_value")
	// 属性带中文说明（jsonschema tag → description）
	textProp, _ := props["text"].(map[string]any)
	assert.Contains(t, textProp["description"], "文本内容")
}

// TestGuestScope 访客作用域（2026-10-12）：tools/list 只见快速分享工具，
// 管理员工具越权调用被拒（列表过滤之外的复核）。
func TestGuestScope(t *testing.T) {
	s := newMCPTestService(t)
	guest := Caller{} // 未登录

	_, out := rpcAs(t, s, guest, "tools/list", nil)
	tools := out["result"].(map[string]any)["tools"].([]any)
	names := []string{}
	for _, tl := range tools {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	assert.ElementsMatch(t, []string{"share_text", "share_file"}, names, "游客仅快速分享工具")

	// 越权调用管理员工具（即使直接 tools/call）
	_, out = rpcAs(t, s, guest, "tools/call", map[string]any{"name": "list_shares"})
	assert.True(t, toolIsErr(out), "越权调用应为工具执行错误")
	assert.Contains(t, toolErrText(t, out), "该工具不可用")

	// 登录用户可见寄件码/预览但不可见管理工具
	user := Caller{UserID: 7}
	_, out = rpcAs(t, s, user, "tools/list", nil)
	userNames := map[string]bool{}
	for _, tl := range out["result"].(map[string]any)["tools"].([]any) {
		userNames[tl.(map[string]any)["name"].(string)] = true
	}
	assert.True(t, userNames["create_request_code"])
	assert.True(t, userNames["preview_share"])
	assert.False(t, userNames["delete_share"])
}

// TestGuestShareText_MarkdownAndCustomCodeStripped 游客快速分享：结果带 Markdown
// 链接；custom_code 仅登录用户可用（游客传了也忽略，防匿名铸造好记码）。
func TestGuestShareText_MarkdownAndCustomCodeStripped(t *testing.T) {
	s := newMCPTestService(t)
	_, out := rpcAs(t, s, Caller{IP: "10.0.0.9"}, "tools/call", map[string]any{
		"name": "share_text",
		"arguments": map[string]any{
			"text":        "guest quick share",
			"custom_code": "MyCode123",
		},
	})
	assert.False(t, toolIsErr(out), "游客快速分享应成功: %s", toolErrText(t, out))
	o := toolOut(t, out)
	code, _ := o["code"].(string)
	assert.Len(t, code, 8)
	assert.NotEqual(t, "MyCode123", code, "游客自定义取件码必须被忽略")
	markdown, _ := o["markdown"].(string)
	assert.Contains(t, markdown, "](")
	shareURL, _ := o["share_url"].(string)
	assert.NotEmpty(t, shareURL)
}

// fakeRequestAPI 寄件码工具测试桩。
type fakeRequestAPI struct{ token string }

func (f fakeRequestAPI) Create(_ context.Context, _ uint, _ RequestCreate) (string, time.Time, error) {
	return f.token, time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC), nil
}

// fakePreviewAPI 预览工具测试桩。
type fakePreviewAPI struct{ result *PreviewResult }

func (f fakePreviewAPI) Preview(_ context.Context, _ PreviewReq) (*PreviewResult, error) {
	return f.result, nil
}

// TestCreateRequestCode 寄件码工具（登录用户）：返回投递链接。
func TestCreateRequestCode(t *testing.T) {
	s := newMCPTestService(t)
	s.SetRequestService(fakeRequestAPI{token: strings.Repeat("a", 32)})
	s.SetBaseURL("http://test.local")

	_, out := rpcAs(t, s, Caller{UserID: 7}, "tools/call", map[string]any{
		"name":      "create_request_code",
		"arguments": map[string]any{"title": "给我投文件"},
	})
	assert.False(t, toolIsErr(out))
	o := toolOut(t, out)
	assert.Equal(t, "http://test.local/#/request/"+strings.Repeat("a", 32), o["delivery_url"])
	assert.Equal(t, strings.Repeat("a", 32), o["token"])
	assert.NotEmpty(t, o["expires_at"])

	// 游客不可用
	_, out = rpcAs(t, s, Caller{}, "tools/call", map[string]any{"name": "create_request_code"})
	assert.True(t, toolIsErr(out))
	assert.Contains(t, toolErrText(t, out), "该工具不可用")
}

// TestPreviewShare 预览工具：文本摘要输出 + 游客不可用。
func TestPreviewShare(t *testing.T) {
	s := newMCPTestService(t)
	s.SetPreviewService(fakePreviewAPI{result: &PreviewResult{
		PreviewType: "text", MimeType: "text/plain", TextContent: "hello preview", FileSize: 13,
	}})

	_, out := rpcAs(t, s, Caller{UserID: 7}, "tools/call", map[string]any{
		"name": "preview_share", "arguments": map[string]any{"code": "whatever"},
	})
	assert.False(t, toolIsErr(out))
	o := toolOut(t, out)
	assert.Equal(t, "hello preview", o["text"])
	assert.Equal(t, "text", o["preview_type"])
	assert.Equal(t, "text/plain", o["mime_type"])

	_, out = rpcAs(t, s, Caller{}, "tools/call", map[string]any{
		"name": "preview_share", "arguments": map[string]any{"code": "whatever"},
	})
	assert.True(t, toolIsErr(out))
	assert.Contains(t, toolErrText(t, out), "该工具不可用")
}

func TestToolsCall_TextShareRoundtrip(t *testing.T) {
	s := newMCPTestService(t)

	// share_text 创建
	_, out := rpc(t, s, "tools/call", map[string]any{
		"name": "share_text",
		"arguments": map[string]any{
			"text":         "mcp-e2e-content",
			"expire_value": 1,
			"expire_style": "day",
		},
	})
	assert.False(t, toolIsErr(out), "share_text 应成功: %s", toolErrText(t, out))
	created := toolOut(t, out)
	code, _ := created["code"].(string)
	require.Len(t, code, 8)
	assert.NotEmpty(t, created["share_url"])

	// get_share 查询
	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "get_share", "arguments": map[string]any{"code": code},
	})
	assert.False(t, toolIsErr(out))
	info := toolOut(t, out)
	assert.Equal(t, code, info["code"])
	assert.Equal(t, "text", info["kind"])

	// list_shares 可见
	_, out = rpc(t, s, "tools/call", map[string]any{"name": "list_shares"})
	assert.False(t, toolIsErr(out))
	listing := toolOut(t, out)
	items, _ := listing["items"].([]any)
	require.NotEmpty(t, items)
	assert.Equal(t, code, items[0].(map[string]any)["code"])

	// delete_share 删除
	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "delete_share", "arguments": map[string]any{"code": code},
	})
	assert.False(t, toolIsErr(out))
	assert.Equal(t, code, toolOut(t, out)["code"])

	// 删除后查不到
	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "get_share", "arguments": map[string]any{"code": code},
	})
	assert.True(t, toolIsErr(out))
}

// TestResolveFileCodeByPickup 管理类工具凭 6 位取件码直查（resolveFileCode：
// pickup_code 列优先语义，含小写输入归一化）+ list_shares 按取件码搜索命中
// （管理面 List LIKE 链含 pickup_code）。
func TestResolveFileCodeByPickup(t *testing.T) {
	s, g := newMCPTestServiceWithDB(t)
	pickup := "ZK3M4N"
	fc := &model.FileCode{Code: "ab12cd34", PickupCode: &pickup}
	require.NoError(t, g.Create(fc).Error)

	// get_share 小写取件码直查（归一化大写后命中 pickup 列）
	_, out := rpc(t, s, "tools/call", map[string]any{
		"name": "get_share", "arguments": map[string]any{"code": "zk3m4n"},
	})
	assert.False(t, toolIsErr(out))
	assert.Equal(t, "ab12cd34", toolOut(t, out)["code"])

	// list_shares 按取件码搜索命中
	_, out = rpc(t, s, "tools/call", map[string]any{"name": "list_shares", "arguments": map[string]any{"keyword": pickup}})
	assert.False(t, toolIsErr(out))
	items, _ := toolOut(t, out)["items"].([]any)
	require.Len(t, items, 1)
	assert.Equal(t, "ab12cd34", items[0].(map[string]any)["code"])

	// 未占用的 6 位码仍报不存在
	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "get_share", "arguments": map[string]any{"code": "ZZZZ99"},
	})
	assert.True(t, toolIsErr(out))
}

func TestToolsCall_TextSharePassword(t *testing.T) {
	s, g := newMCPTestServiceWithDB(t)
	_, out := rpc(t, s, "tools/call", map[string]any{
		"name": "share_text",
		"arguments": map[string]any{
			"text":     "secret",
			"password": "pw123456",
		},
	})
	assert.False(t, toolIsErr(out))
	created := toolOut(t, out)
	code, _ := created["code"].(string)
	require.Len(t, code, 8)
	assert.Equal(t, true, created["has_password"], "出参应标记密码保护")
	// 密码分享不给直链（须先经取件页验密）
	assert.Empty(t, created["download_url"])

	// 密码保护标记落库
	fc, err := dao.NewFileCodeRepository(g).GetByCode(context.Background(), code)
	require.NoError(t, err)
	assert.True(t, fc.RequireAuth)
	assert.NotEmpty(t, fc.PasswordHash)
}

func TestToolsCall_UnknownTool(t *testing.T) {
	s := newMCPTestService(t)
	_, out := rpc(t, s, "tools/call", map[string]any{"name": "nope"})
	// 官方 SDK 语义：未知工具属**协议层**错误（-32602 invalid params），
	// 而非工具执行错误（isError）——工具不存在不是模型可自纠的业务失败。
	require.NotNil(t, out["error"], "未知工具应回协议错误")
	assert.Equal(t, float64(-32602), out["error"].(map[string]any)["code"])
}

// TestToolInputValidation 类型化入参的严格校验（官方 SDK 依 schema 校验）：
// 未知字段、类型错误、非法枚举值一律回工具执行错误，模型可据报错自纠。
func TestToolInputValidation(t *testing.T) {
	s := newMCPTestService(t)

	// 未知字段：schema additionalProperties=false → 拒绝
	_, out := rpc(t, s, "tools/call", map[string]any{
		"name":      "share_text",
		"arguments": map[string]any{"text": "hi", "not_a_field": "x"},
	})
	assert.True(t, toolIsErr(out), "未知入参字段应被拒绝")
	assert.Contains(t, toolErrText(t, out), "not_a_field")

	// 类型错误：expire_value 应为整数
	_, out = rpc(t, s, "tools/call", map[string]any{
		"name":      "share_text",
		"arguments": map[string]any{"text": "hi", "expire_value": "abc"},
	})
	assert.True(t, toolIsErr(out), "类型错误入参应被拒绝")

	// 缺必填项：text 必填
	_, out = rpc(t, s, "tools/call", map[string]any{
		"name":      "share_text",
		"arguments": map[string]any{},
	})
	assert.True(t, toolIsErr(out), "缺必填项应被拒绝")

	// 枚举越界：expire_style 非法值（handler 层校验，给出可读错误）
	_, out = rpc(t, s, "tools/call", map[string]any{
		"name":      "share_text",
		"arguments": map[string]any{"text": "hi", "expire_style": "eon"},
	})
	assert.True(t, toolIsErr(out), "非法 expire_style 应被拒绝")
	assert.Contains(t, toolErrText(t, out), "expire_style")
}

// mapStorage 带内容的存储桩（download_share_file 用；其余方法沿用 nopStorage）
type mapStorage struct {
	nopStorage
	data map[string][]byte
}

func (m mapStorage) SaveStream(_ context.Context, p string, r io.Reader, _ int64) (int64, error) {
	d, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	m.data[p] = d
	return int64(len(d)), nil
}
func (m mapStorage) GetFileSize(_ context.Context, p string) (int64, error) {
	return int64(len(m.data[p])), nil
}
func (m mapStorage) GetFile(_ context.Context, p string) ([]byte, error) {
	if d, ok := m.data[p]; ok {
		return d, nil
	}
	return nil, errors.New("not found")
}

func TestToolsCall_FileShareRoundtrip(t *testing.T) {
	s := newMCPTestService(t)

	content := []byte("hello-mcp-file-2026")
	b64 := base64.StdEncoding.EncodeToString(content)

	// share_file 创建
	_, out := rpc(t, s, "tools/call", map[string]any{
		"name": "share_file",
		"arguments": map[string]any{
			"file_name":      "report.txt",
			"content_base64": b64,
			"expire_style":   "day",
		},
	})
	assert.False(t, toolIsErr(out), "share_file 应成功: %s", toolErrText(t, out))
	created := toolOut(t, out)
	code, _ := created["code"].(string)
	require.Len(t, code, 8)
	assert.Equal(t, "report.txt", created["file_name"])
	assert.Equal(t, float64(len(content)), created["size_bytes"])
	assert.NotEmpty(t, created["download_url"], "无密码文件分享应给出直链")

	// get_share_content 读文件清单
	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "get_share_content", "arguments": map[string]any{"code": code},
	})
	assert.False(t, toolIsErr(out))
	contentOut := toolOut(t, out)
	assert.Equal(t, "file", contentOut["kind"])
	assert.Equal(t, "report.txt", contentOut["name"])
	files, _ := contentOut["files"].([]any)
	require.NotEmpty(t, files)
	assert.Equal(t, "report.txt", files[0].(map[string]any)["name"])

	// download_share_file base64 回传与原内容一致
	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "download_share_file", "arguments": map[string]any{"code": code},
	})
	assert.False(t, toolIsErr(out), "download_share_file 应成功: %s", toolErrText(t, out))
	dl := toolOut(t, out)
	assert.Equal(t, "report.txt", dl["file_name"])
	got, err := base64.StdEncoding.DecodeString(dl["content_base64"].(string))
	require.NoError(t, err)
	assert.Equal(t, content, got)

	// delete_share 清理
	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "delete_share", "arguments": map[string]any{"code": code},
	})
	assert.False(t, toolIsErr(out))
}

func TestToolsCall_FileShareRejections(t *testing.T) {
	s := newMCPTestService(t)

	// 扩展名白名单拒绝
	_, out := rpc(t, s, "tools/call", map[string]any{
		"name": "share_file",
		"arguments": map[string]any{
			"file_name":      "evil.exe",
			"content_base64": base64.StdEncoding.EncodeToString([]byte("x")),
		},
	})
	assert.True(t, toolIsErr(out), "exe 应被白名单拒绝")

	// 非法 base64
	_, out = rpc(t, s, "tools/call", map[string]any{
		"name":      "share_file",
		"arguments": map[string]any{"file_name": "a.txt", "content_base64": "!!not-b64!!"},
	})
	assert.True(t, toolIsErr(out))

	// 文本分享走下载被引导到 get_share_content
	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "share_text", "arguments": map[string]any{"text": "just-text"},
	})
	assert.False(t, toolIsErr(out))
	code, _ := toolOut(t, out)["code"].(string)
	require.Len(t, code, 8)
	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "download_share_file", "arguments": map[string]any{"code": code},
	})
	assert.True(t, toolIsErr(out))
	assert.Contains(t, toolErrText(t, out), "get_share_content")

	// 超过 MCP 上限拒绝
	g := newMCPTestDB(t)
	tiny := NewService(g, "test")
	tiny.SetShareService(shareAPIAdapter{shareApp.NewService(g, "http://test.local", nopStorage{})})
	tiny.SetStorageService(mapStorage{data: map[string][]byte{}})
	tiny.SetMaxFileSize(4)
	_, out = rpc(t, tiny, "tools/call", map[string]any{
		"name": "share_file",
		"arguments": map[string]any{
			"file_name":      "big.txt",
			"content_base64": base64.StdEncoding.EncodeToString([]byte("12345678")),
		},
	})
	assert.True(t, toolIsErr(out))
	assert.Contains(t, toolErrText(t, out), "上限")
}

func TestToolsCall_TextContent(t *testing.T) {
	s := newMCPTestService(t)
	_, out := rpc(t, s, "tools/call", map[string]any{
		"name": "share_text", "arguments": map[string]any{"text": "正文内容-abc123"},
	})
	assert.False(t, toolIsErr(out))
	code, _ := toolOut(t, out)["code"].(string)
	require.Len(t, code, 8)

	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "get_share_content", "arguments": map[string]any{"code": code},
	})
	assert.False(t, toolIsErr(out))
	assert.Equal(t, "正文内容-abc123", toolOut(t, out)["text"])
}

func TestToolsCall_Federation(t *testing.T) {
	s := newMCPTestService(t)

	// 未启用：status 是事实陈述（isError=false），resolve 报错
	_, out := rpc(t, s, "tools/call", map[string]any{"name": "federation_status"})
	assert.False(t, toolIsErr(out))
	assert.Equal(t, false, toolOut(t, out)["enabled"])

	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "federation_resolve", "arguments": map[string]any{"code": "ABCD1234"},
	})
	assert.True(t, toolIsErr(out))

	// 已启用：fake 适配器返回状态与路由
	s.SetFederationService(fakeFederation{})
	_, out = rpc(t, s, "tools/call", map[string]any{"name": "federation_status"})
	assert.False(t, toolIsErr(out))
	status := toolOut(t, out)
	assert.Equal(t, true, status["enabled"])
	assert.Equal(t, "node-abc", status["node_id"])
	registries, _ := status["registries"].([]any)
	require.Len(t, registries, 1)
	assert.Equal(t, "http://registry:12346", registries[0])

	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "federation_resolve", "arguments": map[string]any{"code": "ABCD1234"},
	})
	assert.False(t, toolIsErr(out))
	resolved := toolOut(t, out)
	assert.Equal(t, true, resolved["found"])
	assert.Equal(t, "peer-node", resolved["node_id"])
}

type fakeFederation struct{}

func (fakeFederation) Status() FederationStatus {
	return FederationStatus{Enabled: true, NodeID: "node-abc", Healthy: true,
		Registries: []string{"http://registry:12346"}}
}

func (fakeFederation) Resolve(code string) (*FederationResolve, error) {
	if code == "ABCD1234" {
		return &FederationResolve{NodeID: "peer-node", URL: "https://peer.example.com",
			Name: "peer", ExpiresAt: time.Now().Add(24 * time.Hour), SizeHint: 1024}, nil
	}
	return nil, nil
}

func TestProtocolErrors(t *testing.T) {
	s := newMCPTestService(t)

	// 非法 JSON → HTTP 400（官方 SDK 在传输层拒绝，不进 JSON-RPC 层）
	st, _ := postRPC(t, s, Caller{Admin: true}, []byte("{not-json"))
	assert.Equal(t, 400, st)

	// 未知方法 → 官方 SDK 在传输层拒绝（HTTP 400 + 纯文本），不进 JSON-RPC 层
	st, raw := postRPC(t, s, Caller{Admin: true}, []byte(`{"jsonrpc":"2.0","id":1,"method":"foo/bar"}`))
	assert.Equal(t, 400, st)
	assert.Contains(t, string(raw), "foo/bar")

	// ping → 空对象
	_, out := rpc(t, s, "ping", nil)
	assert.NotNil(t, out["result"])
}

// ---- P2P 联邦工具（Wave B'：直传四工具退役，剩三工具）----

// stubP2p mcp.P2pAPI 测试桩。
type stubP2p struct {
	acceptedID string
}

func (s *stubP2p) P2pPeers(ctx context.Context) ([]P2pPeerInfo, error) {
	return []P2pPeerInfo{
		{NodeID: strings.Repeat("ab", 32), Name: "对端B", URL: "", BoxPub: strings.Repeat("cd", 32), OnlineSince: 1792000000},
	}, nil
}
func (s *stubP2p) P2pInvites(ctx context.Context) ([]P2pInviteInfo, error) {
	return []P2pInviteInfo{
		{ID: "invite000000001", Code: "KJ8Y-WCYZ-E249", FromName: "远端站", Note: "季度备份", ExpiresIn: 540},
	}, nil
}
func (s *stubP2p) P2pAccept(ctx context.Context, inviteID string) (P2pInviteInfo, error) {
	s.acceptedID = inviteID
	return P2pInviteInfo{
		ID: inviteID, Code: "KJ8Y-WCYZ-E249", FromName: "远端站",
		Note: "季度备份", ExpiresIn: 540,
	}, nil
}

// TestP2pTools 联邦三工具：admin 作用域；未注入引导态；注入后可调用；游客不可见不可调。
func TestP2pTools(t *testing.T) {
	s := newMCPTestService(t)

	// 1) 未注入 → 引导态（空列表）而非 panic
	_, out := rpc(t, s, "tools/call", map[string]any{"name": "p2p_peers"})
	assert.False(t, toolIsErr(out))
	peers, _ := toolOut(t, out)["peers"].([]any)
	assert.Empty(t, peers)

	// 2) 注入桩后 peers/invites/accept 全通
	stub := &stubP2p{}
	s.SetP2pService(stub)

	_, out = rpc(t, s, "tools/call", map[string]any{"name": "p2p_peers"})
	assert.False(t, toolIsErr(out))
	peers, _ = toolOut(t, out)["peers"].([]any)
	require.Len(t, peers, 1)
	assert.Equal(t, "对端B", peers[0].(map[string]any)["name"])
	assert.Equal(t, strings.Repeat("ab", 32), peers[0].(map[string]any)["node_id"])

	_, out = rpc(t, s, "tools/call", map[string]any{"name": "p2p_invites"})
	assert.False(t, toolIsErr(out))
	invites, _ := toolOut(t, out)["invites"].([]any)
	require.Len(t, invites, 1)
	assert.Equal(t, "KJ8Y-WCYZ-E249", invites[0].(map[string]any)["code"])
	assert.Equal(t, "远端站", invites[0].(map[string]any)["from_name"])

	// 3) 认领：纯信封（出箱 + 返回信封），不再发起直传收件
	_, out = rpc(t, s, "tools/call", map[string]any{"name": "p2p_accept", "arguments": map[string]any{
		"invite_id": "invite000000001",
	}})
	assert.False(t, toolIsErr(out))
	accepted := toolOut(t, out)
	assert.Equal(t, "KJ8Y-WCYZ-E249", accepted["code"])
	assert.Equal(t, "远端站", accepted["from_name"])
	assert.Equal(t, "invite000000001", stub.acceptedID)

	// 3.5) 参数校验：accept 无 invite_id → 工具执行错误
	_, out = rpc(t, s, "tools/call", map[string]any{"name": "p2p_accept", "arguments": map[string]any{}})
	assert.True(t, toolIsErr(out))

	// 4) 游客：工具列表不可见 + 直接调用被拒
	_, out = rpcAs(t, s, Caller{IP: "10.0.0.9"}, "tools/list", nil)
	for _, tl := range out["result"].(map[string]any)["tools"].([]any) {
		name := tl.(map[string]any)["name"].(string)
		assert.NotContains(t, name, "p2p_", "游客不应可见 %s", name)
	}
	_, out = rpcAs(t, s, Caller{IP: "10.0.0.9"}, "tools/call", map[string]any{"name": "p2p_peers"})
	assert.True(t, toolIsErr(out))
	assert.Contains(t, toolErrText(t, out), "该工具不可用")
}
