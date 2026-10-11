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
	requireAuth bool, passwordHash string, ownerIP, customCode string) (string, string, error) {
	resp, err := a.svc.ShareTextWithAuth(ctx, text, expireValue, expireStyle, requireAuth, passwordHash, nil, ownerIP, false, customCode)
	if err != nil {
		return "", "", err
	}
	return resp.Code, resp.FullShareURL, nil
}

func (a shareAPIAdapter) ShareBytes(ctx context.Context, opts FileShareOpts) (string, string, error) {
	resp, err := a.svc.ShareBytes(ctx, shareApp.ShareBytesOpts{
		FileName: opts.FileName, Content: opts.Content,
		ExpireValue: opts.ExpireValue, ExpireStyle: opts.ExpireStyle,
		PasswordHash: opts.PasswordHash, CustomCode: opts.CustomCode,
	})
	if err != nil {
		return "", "", err
	}
	return resp.Code, resp.FullShareURL, nil
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

// rpc 发送 JSON-RPC 请求并解析响应
func rpc(t *testing.T, s *Service, method string, params any) (status int, out map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	st, raw := s.Handle(context.Background(), body)
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
	st, raw := s.Handle(context.Background(), body)
	assert.Equal(t, 202, st)
	assert.Empty(t, raw, "通知不返回响应体")
}

func TestToolsList_ThirteenTools(t *testing.T) {
	s := newMCPTestService(t)
	_, out := rpc(t, s, "tools/list", nil)
	tools := out["result"].(map[string]any)["tools"].([]any)
	assert.Len(t, tools, 13)
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"share_text", "share_file", "get_share", "get_share_content",
		"download_share_file", "list_shares", "delete_share",
		"get_system_status", "get_storage_info", "list_users", "cleanup_expired",
		"federation_status", "federation_resolve"} {
		assert.True(t, names[want], "缺少工具 %s", want)
	}
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

	// 提取取件码（"取件码: XXXXXXXX"）
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
	marker := "取件码: "
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
	st, raw := s.Handle(context.Background(), []byte("{not-json"))
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
