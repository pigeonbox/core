package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
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
	resp, err := a.svc.ShareTextWithAuth(ctx, text, expireValue, expireStyle, requireAuth, passwordHash, userID, ownerIP, false, customCode)
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
	require.NoError(t, g.AutoMigrate(&model.FileCode{}, &model.User{}, &model.AdminOperationLog{}))
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
	st, raw := s.Handle(context.Background(), body, caller)
	require.NotEmpty(t, raw, "%s 应返回响应体", method)
	require.NoError(t, json.Unmarshal(raw, &out))
	return st, out
}

func TestInitialize(t *testing.T) {
	s := newMCPTestService(t)
	st, out := rpc(t, s, "initialize", map[string]any{
		"protocolVersion": "2025-03-26",
		"clientInfo":      map[string]any{"name": "test-client", "version": "0"},
	})
	assert.Equal(t, 200, st)
	result := out["result"].(map[string]any)
	assert.Equal(t, protocolVersion, result["protocolVersion"])
	assert.Equal(t, serverName, result["serverInfo"].(map[string]any)["name"])
	assert.Contains(t, result["capabilities"].(map[string]any), "tools")
}

func TestNotification_NoResponse(t *testing.T) {
	s := newMCPTestService(t)
	body := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	st, raw := s.Handle(context.Background(), body, Caller{Admin: true})
	assert.Equal(t, 202, st)
	assert.Empty(t, raw, "通知不返回响应体")
}

func TestToolsList_AllTools(t *testing.T) {
	s := newMCPTestService(t)
	_, out := rpc(t, s, "tools/list", nil)
	tools := out["result"].(map[string]any)["tools"].([]any)
	assert.Len(t, tools, 22)
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"share_text", "share_file", "create_request_code", "preview_share",
		"get_share", "get_share_content",
		"download_share_file", "list_shares", "delete_share",
		"get_system_status", "get_storage_info", "list_users", "cleanup_expired",
		"federation_status", "federation_resolve",
		"p2p_status", "p2p_tasks", "p2p_send", "p2p_recv"} {
		assert.True(t, names[want], "缺少工具 %s", want)
	}
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
	content := out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	assert.Contains(t, content, "该工具不可用")

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
	result := out["result"].(map[string]any)
	assert.NotEqual(t, true, result["isError"])
	content := result["content"].([]any)[0].(map[string]any)["text"].(string)
	assert.Contains(t, content, "Markdown: [")
	code := extractCode(content)
	assert.NotEqual(t, "MyCode123", code, "游客自定义取件码必须被忽略")
	assert.Len(t, code, 8)
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
	content := out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	assert.Contains(t, content, "http://test.local/#/request/"+strings.Repeat("a", 32))

	// 游客不可用
	_, out = rpcAs(t, s, Caller{}, "tools/call", map[string]any{"name": "create_request_code"})
	content = out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	assert.Contains(t, content, "该工具不可用")
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
	content := out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	assert.Contains(t, content, "hello preview")

	_, out = rpcAs(t, s, Caller{}, "tools/call", map[string]any{
		"name": "preview_share", "arguments": map[string]any{"code": "whatever"},
	})
	content = out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	assert.Contains(t, content, "该工具不可用")
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
	result := out["result"].(map[string]any)
	assert.NotEqual(t, true, result["isError"])
	content := result["content"].([]any)[0].(map[string]any)["text"].(string)
	assert.Contains(t, content, "分享创建成功")

	// 提取分享码（"分享码: XXXXXXXX"，get_share 等按分享码查询）
	code := extractCode(content)
	require.Len(t, code, 8)

	// get_share 查询
	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "get_share", "arguments": map[string]any{"code": code},
	})
	info := out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	assert.Contains(t, info, code)
	assert.Contains(t, info, "文本")

	// list_shares 可见
	_, out = rpc(t, s, "tools/call", map[string]any{"name": "list_shares"})
	listing := out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	assert.Contains(t, listing, code)

	// delete_share 删除
	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "delete_share", "arguments": map[string]any{"code": code},
	})
	deleted := out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	assert.Contains(t, deleted, "已删除")

	// 删除后查不到
	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "get_share", "arguments": map[string]any{"code": code},
	})
	assert.Equal(t, true, out["result"].(map[string]any)["isError"])
}

func extractCode(content string) string {
	marker := "分享码: "
	idx := strings.Index(content, marker)
	if idx < 0 {
		return ""
	}
	rest := content[idx+len(marker):]
	if len(rest) < 8 {
		return ""
	}
	return rest[:8]
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
	content := out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	code := extractCode(content)
	require.Len(t, code, 8)

	// 密码保护标记落库
	fc, err := dao.NewFileCodeRepository(g).GetByCode(context.Background(), code)
	require.NoError(t, err)
	assert.True(t, fc.RequireAuth)
	assert.NotEmpty(t, fc.PasswordHash)
}

func TestToolsCall_UnknownTool(t *testing.T) {
	s := newMCPTestService(t)
	_, out := rpc(t, s, "tools/call", map[string]any{"name": "nope"})
	assert.Equal(t, true, out["result"].(map[string]any)["isError"])
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

// toolText 取 tools/call 响应的文本内容
func toolText(t *testing.T, out map[string]any) (string, bool) {
	t.Helper()
	result := out["result"].(map[string]any)
	isErr, _ := result["isError"].(bool)
	return result["content"].([]any)[0].(map[string]any)["text"].(string), isErr
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
	text, isErr := toolText(t, out)
	assert.False(t, isErr, "share_file 应成功: %s", text)
	code := extractCode(text)
	require.Len(t, code, 8)
	assert.Contains(t, text, "report.txt")

	// get_share_content 读文件清单
	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "get_share_content", "arguments": map[string]any{"code": code},
	})
	text, isErr = toolText(t, out)
	assert.False(t, isErr)
	assert.Contains(t, text, "report.txt")
	assert.Contains(t, text, "文件分享")

	// download_share_file base64 回传与原内容一致
	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "download_share_file", "arguments": map[string]any{"code": code},
	})
	text, isErr = toolText(t, out)
	assert.False(t, isErr, "download_share_file 应成功: %s", text)
	idx := strings.Index(text, "base64:\n")
	require.GreaterOrEqual(t, idx, 0)
	got, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text[idx+len("base64:\n"):]))
	require.NoError(t, err)
	assert.Equal(t, content, got)

	// delete_share 清理
	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "delete_share", "arguments": map[string]any{"code": code},
	})
	_, isErr = toolText(t, out)
	assert.False(t, isErr)
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
	_, isErr := toolText(t, out)
	assert.True(t, isErr, "exe 应被白名单拒绝")

	// 非法 base64
	_, out = rpc(t, s, "tools/call", map[string]any{
		"name":      "share_file",
		"arguments": map[string]any{"file_name": "a.txt", "content_base64": "!!not-b64!!"},
	})
	_, isErr = toolText(t, out)
	assert.True(t, isErr)

	// 文本分享走下载被引导到 get_share_content
	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "share_text", "arguments": map[string]any{"text": "just-text"},
	})
	text, _ := toolText(t, out)
	code := extractCode(text)
	require.Len(t, code, 8)
	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "download_share_file", "arguments": map[string]any{"code": code},
	})
	text, isErr = toolText(t, out)
	assert.True(t, isErr)
	assert.Contains(t, text, "get_share_content")

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
	text, isErr = toolText(t, out)
	assert.True(t, isErr)
	assert.Contains(t, text, "上限")
}

func TestToolsCall_TextContent(t *testing.T) {
	s := newMCPTestService(t)
	_, out := rpc(t, s, "tools/call", map[string]any{
		"name": "share_text", "arguments": map[string]any{"text": "正文内容-abc123"},
	})
	text, _ := toolText(t, out)
	code := extractCode(text)
	require.Len(t, code, 8)

	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "get_share_content", "arguments": map[string]any{"code": code},
	})
	text, isErr := toolText(t, out)
	assert.False(t, isErr)
	assert.Contains(t, text, "正文内容-abc123")
}

func TestToolsCall_Federation(t *testing.T) {
	s := newMCPTestService(t)

	// 未启用：status 是事实陈述（isError=false），resolve 报错
	_, out := rpc(t, s, "tools/call", map[string]any{"name": "federation_status"})
	text, isErr := toolText(t, out)
	assert.False(t, isErr)
	assert.Contains(t, text, "未启用")

	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "federation_resolve", "arguments": map[string]any{"code": "ABCD1234"},
	})
	_, isErr = toolText(t, out)
	assert.True(t, isErr)

	// 已启用：fake 适配器返回状态与路由
	s.SetFederationService(fakeFederation{})
	_, out = rpc(t, s, "tools/call", map[string]any{"name": "federation_status"})
	text, isErr = toolText(t, out)
	assert.False(t, isErr)
	assert.Contains(t, text, "node-abc")
	assert.Contains(t, text, "http://registry:12346")

	_, out = rpc(t, s, "tools/call", map[string]any{
		"name": "federation_resolve", "arguments": map[string]any{"code": "ABCD1234"},
	})
	text, isErr = toolText(t, out)
	assert.False(t, isErr)
	assert.Contains(t, text, "peer-node")
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

	// 非法 JSON → -32700
	st, raw := s.Handle(context.Background(), []byte("{not-json"), Caller{Admin: true})
	assert.Equal(t, 200, st)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(raw, &resp))
	assert.Equal(t, float64(errParse), resp["error"].(map[string]any)["code"])

	// 未知方法 → -32601
	st, out := rpc(t, s, "resources/list", nil)
	assert.Equal(t, 200, st)
	assert.Equal(t, float64(errMethodNF), out["error"].(map[string]any)["code"])

	// ping → 空对象
	_, out = rpc(t, s, "ping", nil)
	assert.NotNil(t, out["result"])
}

// ---- P2P 设备直传工具（P2b）----

// stubP2p mcp.P2pAPI 测试桩。
type stubP2p struct {
	sentPaths  []string
	gotCode    string
	inviteTo   string
	inviteNote string
	acceptedID string
}

func (s *stubP2p) P2pStatus(ctx context.Context) (P2pStatus, error) {
	return P2pStatus{
		Available: true, Enabled: true, DeployAllowed: true,
		BinaryPath: "/app/p2pc", RegistryURL: "http://r:56788", RecvDir: "/data/inbox",
		InboxUsed: 1024, InboxQuota: 4096,
	}, nil
}
func (s *stubP2p) P2pTasks(ctx context.Context) ([]P2pTaskInfo, error) {
	return []P2pTaskInfo{
		{ID: "aaaaaaaaaaaaaaaa", Kind: "send", State: "waiting", Code: "ABCD-1234-WXYZ", CreatedAt: "2026-10-14T10:00:00Z"},
		{ID: "bbbbbbbbbbbbbbbb", Kind: "recv", State: "done", BytesDone: 29, CreatedAt: "2026-10-14T10:01:00Z"},
	}, nil
}
func (s *stubP2p) P2pSend(ctx context.Context, paths []string, inviteTo, inviteNote string) (P2pTaskInfo, error) {
	s.sentPaths = paths
	s.inviteTo, s.inviteNote = inviteTo, inviteNote
	return P2pTaskInfo{ID: "cccccccccccccccc", Kind: "send", State: "waiting", Code: "ABCD-1234-WXYZ"}, nil
}
func (s *stubP2p) P2pRecv(ctx context.Context, code string) (P2pTaskInfo, error) {
	s.gotCode = code
	return P2pTaskInfo{ID: "dddddddddddddddd", Kind: "recv", State: "waiting", Code: code}, nil
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
func (s *stubP2p) P2pAccept(ctx context.Context, inviteID string) (P2pTaskInfo, error) {
	s.acceptedID = inviteID
	return P2pTaskInfo{ID: "eeeeeeeeeeeeeeee", Kind: "recv", State: "waiting", Code: "KJ8Y-WCYZ-E249"}, nil
}

// TestP2pTools 直传四工具：admin 作用域；未注入引导态；注入后可调用；游客不可见不可调。
func TestP2pTools(t *testing.T) {
	s := newMCPTestService(t)

	// 1) 未注入 → 引导态而非 panic
	_, out := rpc(t, s, "tools/call", map[string]any{"name": "p2p_status"})
	content := out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	assert.Contains(t, content, "直传")

	// 2) 注入桩后 status/tasks/send/recv 全通
	stub := &stubP2p{}
	s.SetP2pService(stub)
	_, out = rpc(t, s, "tools/call", map[string]any{"name": "p2p_status"})
	content = out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	assert.Contains(t, content, "启用: 是")
	assert.Contains(t, content, "http://r:56788")

	_, out = rpc(t, s, "tools/call", map[string]any{"name": "p2p_tasks"})
	content = out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	assert.Contains(t, content, "ABCD-1234-WXYZ")
	assert.Contains(t, content, "done")

	_, out = rpc(t, s, "tools/call", map[string]any{"name": "p2p_send", "arguments": map[string]any{
		"paths": []any{"/data/share/a.txt"},
	}})
	content = out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	assert.Contains(t, content, "ABCD-1234-WXYZ")
	assert.Equal(t, []string{"/data/share/a.txt"}, stub.sentPaths)

	_, out = rpc(t, s, "tools/call", map[string]any{"name": "p2p_recv", "arguments": map[string]any{
		"code": "kj8y-wcyz-e249",
	}})
	content = out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	assert.Contains(t, content, "KJ8Y-WCYZ-E249")
	assert.Equal(t, "KJ8Y-WCYZ-E249", stub.gotCode, "口令应归一化大写")

	// 3) 参数校验：send 无 paths / recv 无 code → isError
	_, out = rpc(t, s, "tools/call", map[string]any{"name": "p2p_send", "arguments": map[string]any{}})
	assert.Equal(t, true, out["result"].(map[string]any)["isError"])
	_, out = rpc(t, s, "tools/call", map[string]any{"name": "p2p_recv", "arguments": map[string]any{"code": "  "}})
	assert.Equal(t, true, out["result"].(map[string]any)["isError"])

	// 3.5) 名录/邀请/认领 + send 邀请参数透传
	_, out = rpc(t, s, "tools/call", map[string]any{"name": "p2p_peers"})
	content = out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	assert.Contains(t, content, "对端B")
	assert.Contains(t, content, strings.Repeat("ab", 32))

	_, out = rpc(t, s, "tools/call", map[string]any{"name": "p2p_invites"})
	content = out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	assert.Contains(t, content, "KJ8Y-WCYZ-E249")
	assert.Contains(t, content, "远端站")

	_, out = rpc(t, s, "tools/call", map[string]any{"name": "p2p_accept", "arguments": map[string]any{
		"invite_id": "invite000000001",
	}})
	content = out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	assert.Contains(t, content, "已认领")
	assert.Equal(t, "invite000000001", stub.acceptedID)

	_, out = rpc(t, s, "tools/call", map[string]any{"name": "p2p_send", "arguments": map[string]any{
		"paths":       []any{"/data/share/a.txt"},
		"invite_to":   strings.Repeat("CD", 32),
		"invite_note": "走查附言",
	}})
	content = out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	assert.Contains(t, content, "密封邀请")
	assert.Equal(t, strings.ToLower(strings.Repeat("CD", 32)), stub.inviteTo)
	assert.Equal(t, "走查附言", stub.inviteNote)

	_, out = rpc(t, s, "tools/call", map[string]any{"name": "p2p_accept", "arguments": map[string]any{}})
	assert.Equal(t, true, out["result"].(map[string]any)["isError"])

	// 4) 游客：工具列表不可见 + 直接调用被拒
	_, out = rpcAs(t, s, Caller{IP: "10.0.0.9"}, "tools/list", nil)
	for _, tl := range out["result"].(map[string]any)["tools"].([]any) {
		name := tl.(map[string]any)["name"].(string)
		assert.NotContains(t, name, "p2p_", "游客不应可见 %s", name)
	}
	_, out = rpcAs(t, s, Caller{IP: "10.0.0.9"}, "tools/call", map[string]any{"name": "p2p_status"})
	assert.Equal(t, true, out["result"].(map[string]any)["isError"])
}
