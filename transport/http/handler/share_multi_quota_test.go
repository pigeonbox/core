// multi-bind 配额预检的申报大小口径（2026-10-25 审计 A4）
//
// 缺陷：申报大小只累加 **upload_id** 条目的 chunk 会话大小，**object_key 条目
// 贡献 0** ⇒ 匿名日字节配额 / 登录用户单次上传上限**完全看不到已直传对象**
// ⇒ 绑定 N 个大对象可绕过配额（其真实大小在第二遍、**过配额闸之后**才取）。
//
// 断言必须能变红（负对照）：把 collectMultiBindDeclaredSize 里的 object_key
// 分支去掉（回到「只算 upload_id」）⇒ 本用例红。
package handler

import (
	"context"
	"os"

	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/app/chunk"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/pigeonbox/core/storage"
)

// sizeOnlyStorage 只实现 GetFileSize 的最小 storage 桩（其余方法 panic/忽略）。
type sizeOnlyStorage struct {
	storage.StorageInterface
	sizes map[string]int64
}

func (s *sizeOnlyStorage) GetFileSize(_ context.Context, filePath string) (int64, error) {
	if v, ok := s.sizes[filePath]; ok {
		return v, nil
	}
	return 0, os.ErrNotExist
}

// newQuotaChunkSvc 构造带一条 upload 会话的 chunk 服务。
func newQuotaChunkSvc(t *testing.T, uploadID string, fileSize int64) *chunk.Service {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.UploadChunk{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, g.Create(&model.UploadChunk{
		UploadID:    uploadID,
		ChunkIndex:  -1,
		FileName:    "chunk.bin",
		FileSize:    fileSize,
		TotalChunks: 1,
		Status:      "completed",
	}).Error)
	return chunk.NewService(g)
}

// TestCollectMultiBindDeclaredSize_CountsObjectKey object_key 条目的真实大小
// **必须**计入申报大小（否则配额被绕过）。
func TestCollectMultiBindDeclaredSize_CountsObjectKey(t *testing.T) {
	ctx := context.Background()
	st := &sizeOnlyStorage{sizes: map[string]int64{
		"uploads/2026/10/25/big.bin": 500 * 1024 * 1024, // 500MB
	}}
	chunkSvc := newQuotaChunkSvc(t, "UP-1", 0)

	declared, err := collectMultiBindDeclaredSize(ctx, st, chunkSvc, []multiBindEntry{
		{ObjectKey: "uploads/2026/10/25/big.bin", FileName: "big.bin"},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(500*1024*1024), declared,
		"object_key 条目的真实大小必须计入申报大小（修前贡献 0 ⇒ 配额被绕过）")
}

// TestCollectMultiBindDeclaredSize_CountsUploadID upload_id 条目按会话申报大小计。
func TestCollectMultiBindDeclaredSize_CountsUploadID(t *testing.T) {
	ctx := context.Background()
	st := &sizeOnlyStorage{sizes: map[string]int64{}}
	chunkSvc := newQuotaChunkSvc(t, "UP-1", 1024)

	declared, err := collectMultiBindDeclaredSize(ctx, st, chunkSvc, []multiBindEntry{
		{UploadID: "UP-1", FileName: "chunk.bin"},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1024), declared)
}

// TestCollectMultiBindDeclaredSize_Mixed 混合条目 ⇒ 两类都计入。
func TestCollectMultiBindDeclaredSize_Mixed(t *testing.T) {
	ctx := context.Background()
	st := &sizeOnlyStorage{sizes: map[string]int64{
		"uploads/2026/10/25/big.bin": 2048,
	}}
	chunkSvc := newQuotaChunkSvc(t, "UP-1", 1024)

	declared, err := collectMultiBindDeclaredSize(ctx, st, chunkSvc, []multiBindEntry{
		{UploadID: "UP-1", FileName: "chunk.bin"},
		{ObjectKey: "uploads/2026/10/25/big.bin", FileName: "big.bin"},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1024+2048), declared)
}

// TestCollectMultiBindDeclaredSize_RejectsEmptyEntry 两种来源都缺 ⇒ 明确报错。
func TestCollectMultiBindDeclaredSize_RejectsEmptyEntry(t *testing.T) {
	ctx := context.Background()
	st := &sizeOnlyStorage{sizes: map[string]int64{}}
	chunkSvc := newQuotaChunkSvc(t, "UP-1", 0)

	_, err := collectMultiBindDeclaredSize(ctx, st, chunkSvc, []multiBindEntry{{FileName: "x.bin"}})
	require.Error(t, err, "upload_id/object_key 都缺必须报错")
}

// TestCollectMultiBindDeclaredSize_IgnoresIllegalKey 非法 object_key（路径逃逸 /
// 非 uploads 命名空间）不计入 —— 它会在第二遍被明确拒绝，计入反而是替攻击者
// 探路径。**但**合法 key 必须计入（上一条用例）。
func TestCollectMultiBindDeclaredSize_IgnoresIllegalKey(t *testing.T) {
	ctx := context.Background()
	st := &sizeOnlyStorage{sizes: map[string]int64{
		"etc/passwd":            1024,
		"uploads/../etc/passwd": 1024,
	}}
	chunkSvc := newQuotaChunkSvc(t, "UP-1", 0)

	declared, err := collectMultiBindDeclaredSize(ctx, st, chunkSvc, []multiBindEntry{
		{ObjectKey: "etc/passwd", FileName: "x"},
		{ObjectKey: "uploads/../etc/passwd", FileName: "x"},
	})
	require.NoError(t, err)
	assert.Zero(t, declared, "非法 key 不得计入申报大小")
}

// 目录型 key 不得被当成文件计入（GetFileSize 对目录返回 0/错误）。
func TestCollectMultiBindDeclaredSize_DirKeyIgnored(t *testing.T) {
	ctx := context.Background()
	st := &sizeOnlyStorage{sizes: map[string]int64{}}
	chunkSvc := newQuotaChunkSvc(t, "UP-1", 0)

	declared, err := collectMultiBindDeclaredSize(ctx, st, chunkSvc, []multiBindEntry{
		{ObjectKey: "uploads/2026/10/25/missing.bin", FileName: "x"},
	})
	require.NoError(t, err)
	assert.Zero(t, declared)
}
