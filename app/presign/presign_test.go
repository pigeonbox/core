package presign

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/glebarez/sqlite"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// mockShareService 用于单测的 share service mock
type mockShareService struct {
	mu         sync.Mutex
	called     int
	lastReq    *ShareCreateReq
	returnCode string
	returnURL  string
	returnFull string
	returnErr  error
}

func (m *mockShareService) CreateShare(ctx context.Context, req *ShareCreateReq) (*ShareCreateResp, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.called++
	m.lastReq = req
	if m.returnErr != nil {
		return nil, m.returnErr
	}
	return &ShareCreateResp{
		Code:         m.returnCode,
		ShareURL:     m.returnURL,
		FullShareURL: m.returnFull,
	}, nil
}

// newPresignTestDB 构造注入用内存 sqlite（测试不触 DB 时也保持构造合法）。
func newPresignTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := gormDB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return gormDB
}

// newTestService 创建带 miniredis 的测试 service
func newTestService(t *testing.T) (*Service, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	svc := NewService(newPresignTestDB(t), rdb, "http://test.local", "test-signing-key")
	return svc, mr
}

func TestInit_GeneratesUniqueToken(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		meta := InitMeta{
			FileName:    "test.txt",
			FileSize:    1024,
			ContentType: "text/plain",
		}
		result, err := svc.Init(ctx, meta)
		require.NoError(t, err)
		assert.NotEmpty(t, result.UploadID)
		assert.NotEmpty(t, result.Token)
		assert.True(t, strings.HasPrefix(result.UploadID, "up_"))
		// upload_id 唯一
		assert.False(t, seen[result.UploadID], "upload_id 重复: %s", result.UploadID)
		seen[result.UploadID] = true
	}
}

func TestInit_ResultContainsUploadURL(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	meta := InitMeta{
		FileName:    "test.txt",
		FileSize:    1024,
		ContentType: "text/plain",
	}
	result, err := svc.Init(ctx, meta)
	require.NoError(t, err)
	assert.Contains(t, result.UploadURL, result.UploadID)
	assert.Equal(t, "PUT", result.Method)
	assert.Equal(t, result.Token, result.Headers["X-Upload-Token"])
}

func TestComplete_HappyPath(t *testing.T) {
	svc, _ := newTestService(t)
	mock := &mockShareService{
		returnCode: "shareABC123",
		returnURL:  "/share/shareABC123",
		returnFull: "http://test.local/share/shareABC123",
	}
	svc.SetShareService(mock)

	ctx := context.Background()
	meta := InitMeta{
		FileName:    "test.txt",
		FileSize:    1024,
		ContentType: "text/plain",
		UserID:      0,
		ExpireValue: 1,
		ExpireStyle: "hour",
	}
	result, err := svc.Init(ctx, meta)
	require.NoError(t, err)

	// 调用 Complete
	completed, err := svc.Complete(ctx, result.UploadID, result.Token, "192.168.1.1")
	require.NoError(t, err)
	assert.NotNil(t, completed)
	assert.Equal(t, "shareABC123", completed.ShareCode)
	assert.Equal(t, "/share/shareABC123", completed.ShareURL)
	assert.Equal(t, "http://test.local/share/shareABC123", completed.FullShareURL)
	assert.Equal(t, "test.txt", completed.FileName)
	assert.Equal(t, int64(1024), completed.FileSize)
	assert.Equal(t, "192.168.1.1", completed.OwnerIP)

	// mock share service 被调用 1 次
	assert.Equal(t, 1, mock.called)
	require.NotNil(t, mock.lastReq)
	// ObjectKey 现由服务端生成（uploads/YYYY/MM/DD/<uploadID><ext>，防路径穿越）
	now := time.Now()
	expectedKey := fmt.Sprintf("uploads/%04d/%02d/%02d/%s.txt", now.Year(), int(now.Month()), now.Day(), result.UploadID)
	assert.Equal(t, expectedKey, mock.lastReq.FilePath)
	assert.Equal(t, int64(1024), mock.lastReq.Size)
	assert.Equal(t, "test.txt", mock.lastReq.Text)
	assert.Equal(t, "presign_anonymous", mock.lastReq.UploadType)
	assert.Equal(t, "192.168.1.1", mock.lastReq.OwnerIP)
	assert.Equal(t, result.UploadID, mock.lastReq.UploadID)
}

func TestComplete_WithAuthenticatedUser(t *testing.T) {
	svc, _ := newTestService(t)
	mock := &mockShareService{returnCode: "code1"}
	svc.SetShareService(mock)

	ctx := context.Background()
	meta := InitMeta{
		FileName:    "auth.txt",
		FileSize:    2048,
		ContentType: "text/plain",
		UserID:      42, // 已认证用户
	}
	initRes, err := svc.Init(ctx, meta)
	require.NoError(t, err)

	_, err = svc.Complete(ctx, initRes.UploadID, initRes.Token, "10.0.0.1")
	require.NoError(t, err)
	require.NotNil(t, mock.lastReq)
	assert.Equal(t, "presign_authenticated", mock.lastReq.UploadType)
	require.NotNil(t, mock.lastReq.UserID)
	assert.Equal(t, uint(42), *mock.lastReq.UserID)
}

func TestComplete_InvalidToken(t *testing.T) {
	svc, _ := newTestService(t)
	mock := &mockShareService{returnCode: "shouldNotBeCalled"}
	svc.SetShareService(mock)

	ctx := context.Background()
	meta := InitMeta{
		FileName: "test.txt",
		FileSize: 1024,
	}
	initRes, err := svc.Init(ctx, meta)
	require.NoError(t, err)

	// 错误 token
	_, err = svc.Complete(ctx, initRes.UploadID, "wrong-token", "127.0.0.1")
	assert.ErrorIs(t, err, ErrTokenInvalid)
	// share service 不应被调用
	assert.Equal(t, 0, mock.called)
}

func TestComplete_UploadNotFound(t *testing.T) {
	svc, _ := newTestService(t)
	mock := &mockShareService{returnCode: "x"}
	svc.SetShareService(mock)

	ctx := context.Background()
	_, err := svc.Complete(ctx, "non-existent-id", "any-token", "127.0.0.1")
	assert.ErrorIs(t, err, ErrUploadNotFound)
	assert.Equal(t, 0, mock.called)
}

func TestComplete_AlreadyComplete(t *testing.T) {
	svc, _ := newTestService(t)
	mock := &mockShareService{returnCode: "code"}
	svc.SetShareService(mock)

	ctx := context.Background()
	meta := InitMeta{
		FileName: "test.txt",
		FileSize: 1024,
	}
	initRes, err := svc.Init(ctx, meta)
	require.NoError(t, err)

	// 第一次 Complete 成功
	_, err = svc.Complete(ctx, initRes.UploadID, initRes.Token, "127.0.0.1")
	require.NoError(t, err)

	// 第二次 Complete 失败
	_, err = svc.Complete(ctx, initRes.UploadID, initRes.Token, "127.0.0.1")
	assert.ErrorIs(t, err, ErrAlreadyComplete)
}

func TestComplete_ExpiredUpload(t *testing.T) {
	svc, mr := newTestService(t)
	mock := &mockShareService{returnCode: "x"}
	svc.SetShareService(mock)

	ctx := context.Background()
	meta := InitMeta{
		FileName: "test.txt",
		FileSize: 1024,
	}
	initRes, err := svc.Init(ctx, meta)
	require.NoError(t, err)

	// miniredis 加速时间（meta key TTL = 1h，过期后被自动清理）
	mr.FastForward(2 * time.Hour)

	// Redis 自动清理后 → ErrUploadNotFound
	_, err = svc.Complete(ctx, initRes.UploadID, initRes.Token, "127.0.0.1")
	assert.ErrorIs(t, err, ErrUploadNotFound)
}

func TestComplete_ShareServiceError(t *testing.T) {
	svc, _ := newTestService(t)
	shareErr := errors.New("share db connection lost")
	mock := &mockShareService{returnErr: shareErr}
	svc.SetShareService(mock)

	ctx := context.Background()
	meta := InitMeta{
		FileName: "test.txt",
		FileSize: 1024,
	}
	initRes, err := svc.Init(ctx, meta)
	require.NoError(t, err)

	// Complete 会返回错误（但 InitMeta 已标记 complete）
	completed, err := svc.Complete(ctx, initRes.UploadID, initRes.Token, "127.0.0.1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create share record")
	assert.NotNil(t, completed)
	assert.Empty(t, completed.ShareCode)
	assert.Equal(t, 1, mock.called)
}

func TestAbort(t *testing.T) {
	svc, _ := newTestService(t)
	mock := &mockShareService{returnCode: "x"}
	svc.SetShareService(mock)

	ctx := context.Background()
	meta := InitMeta{
		FileName: "test.txt",
		FileSize: 1024,
	}
	initRes, err := svc.Init(ctx, meta)
	require.NoError(t, err)

	// Abort 成功
	err = svc.Abort(ctx, initRes.UploadID, initRes.Token)
	require.NoError(t, err)

	// 之后 GetMeta 找不到
	_, err = svc.GetMeta(ctx, initRes.UploadID)
	assert.ErrorIs(t, err, ErrUploadNotFound)
	assert.Equal(t, 0, mock.called)
}

func TestAbort_InvalidToken(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	meta := InitMeta{
		FileName: "test.txt",
		FileSize: 1024,
	}
	initRes, err := svc.Init(ctx, meta)
	require.NoError(t, err)

	err = svc.Abort(ctx, initRes.UploadID, "wrong-token")
	assert.ErrorIs(t, err, ErrTokenInvalid)
}

// TestUploadDirect_ReplayAfterCompleteRejected 重放覆盖防线（2026-10-08 加固 P0）：
// Complete 之后 meta 仍存活、令牌未过期，持旧令牌重放 PUT 必须被拒——否则可
// 静默替换已发布分享背后的对象（分享码不变、内容掉包）。
func TestUploadDirect_ReplayAfterCompleteRejected(t *testing.T) {
	svc, _ := newTestService(t)
	mock := &mockShareService{returnCode: "code"}
	svc.SetShareService(mock)

	// nil storage 时 UploadDirect 走 ./data 本地兜底，隔离到临时目录
	oldWd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(t.TempDir()))
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	ctx := context.Background()
	meta := InitMeta{FileName: "test.txt", FileSize: 4}
	initRes, err := svc.Init(ctx, meta)
	require.NoError(t, err)

	// 首次直传成功
	require.NoError(t, svc.UploadDirect(ctx, initRes.UploadID, initRes.Token, strings.NewReader("AAAA"), 4))

	// Complete 后同令牌重放 PUT → ErrAlreadyComplete
	_, err = svc.Complete(ctx, initRes.UploadID, initRes.Token, "127.0.0.1")
	require.NoError(t, err)
	err = svc.UploadDirect(ctx, initRes.UploadID, initRes.Token, strings.NewReader("BBBB"), 4)
	assert.ErrorIs(t, err, ErrAlreadyComplete)
}
