package presign

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newMemoryTestService 构造内存模式（rdb=nil）的 presign service。
func newMemoryTestService(t *testing.T) *Service {
	t.Helper()
	return NewService(newPresignTestDB(t), nil, "http://test.local", "test-signing-key")
}

// TestMemoryMode_IsMemoryKV 构造函数 nil 兜底应进入内存模式。
func TestMemoryMode_IsMemoryKV(t *testing.T) {
	svc := newMemoryTestService(t)
	_, ok := svc.rdb.(memoryKV)
	assert.True(t, ok, "rdb=nil 应兜底为内存 KV")
}

// TestMemoryMode_InitCompleteFlow 内存模式端到端：Init → GetMeta → Complete →
// 二次 Complete 报 AlreadyComplete（覆盖 Get/Set/Del 全部命令面）。
func TestMemoryMode_InitCompleteFlow(t *testing.T) {
	svc := newMemoryTestService(t)
	mock := &mockShareService{
		returnCode: "shareABC123",
		returnURL:  "/#/share/shareABC123",
		returnFull: "http://test.local/#/share/shareABC123",
	}
	svc.SetShareService(mock)

	ctx := context.Background()
	meta := InitMeta{
		FileName:    "test.txt",
		FileSize:    1024,
		ContentType: "text/plain",
	}
	result, err := svc.Init(ctx, meta)
	require.NoError(t, err)
	require.NotEmpty(t, result.UploadID)

	// GetMeta 读回（Get 路径）
	got, err := svc.GetMeta(ctx, result.UploadID)
	require.NoError(t, err)
	assert.Equal(t, result.UploadID, got.UploadID)
	assert.Equal(t, int64(1024), got.FileSize)

	// Complete：读 meta → 标记完成 → 写回（Set 路径）
	res, err := svc.Complete(ctx, result.UploadID, result.Token, "1.2.3.4")
	require.NoError(t, err)
	assert.Equal(t, "shareABC123", res.ShareCode)

	// meta 已标记 complete：二次 Complete 拒绝
	_, err = svc.Complete(ctx, result.UploadID, result.Token, "1.2.3.4")
	assert.ErrorIs(t, err, ErrAlreadyComplete)

	// Abort 删除会话（Del 路径）后 GetMeta 404
	svc2 := newMemoryTestService(t)
	r2, err := svc2.Init(ctx, InitMeta{
		FileName:    "test.txt",
		FileSize:    1024,
		ContentType: "text/plain",
	})
	require.NoError(t, err)
	require.NoError(t, svc2.Abort(ctx, r2.UploadID, r2.Token))
	_, err = svc2.GetMeta(ctx, r2.UploadID)
	assert.ErrorIs(t, err, ErrUploadNotFound)
}

// TestMemoryMode_UploadNotFound 内存模式下未知会话返回与 Redis 模式同义的 NotFound。
func TestMemoryMode_UploadNotFound(t *testing.T) {
	svc := newMemoryTestService(t)
	_, err := svc.GetMeta(context.Background(), "up_missing")
	assert.ErrorIs(t, err, ErrUploadNotFound)
}
