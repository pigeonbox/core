package chunk

import (
	"context"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// =====================================================================
// chunk service 单测（Shape B：DAO 通过全局 db.GetDB() 访问）。
// 注入式：构造直传 *gorm.DB（dao 去全局化终态）。
// =====================================================================

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, gormDB.AutoMigrate(&model.UploadChunk{}))
	// glebarez/sqlite 的 :memory: 每条连接是独立库，多连接会拿到无表空库；
	// 钉死单连接消除该 flake（异步 goroutine 与主流程并发取连接时必现）。
	sqlDB, err := gormDB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return gormDB
}

func newTestService(t *testing.T) *Service {
	t.Helper()
	gormDB := newTestDB(t)
	return NewService(gormDB)
}

// 测试：初始化上传，返回控制记录（chunk_index=-1）
func TestInitiateUpload_Success(t *testing.T) {
	svc := newTestService(t)
	resp, err := svc.InitiateUpload(context.Background(), &InitiateUploadReq{
		UploadID:    "upload-001",
		FileName:    "big.zip",
		TotalChunks: 3,
		FileSize:    30,
		ChunkSize:   10,
	})
	require.NoError(t, err)
	assert.Equal(t, "upload-001", resp.UploadID)
	assert.Equal(t, -1, resp.ChunkIndex) // 控制记录
	assert.Equal(t, 3, resp.TotalChunks)
	assert.Equal(t, "pending", resp.Status)
}

// 测试：重复初始化同一 UploadID → 幂等返回既有进度（供断点续传）。
// 回归：此前直接报错，秒传未命中时同哈希重传（uploadID=fileHash 复用）必 500。
func TestInitiateUpload_DuplicateIdempotent(t *testing.T) {
	svc := newTestService(t)
	req := &InitiateUploadReq{UploadID: "dup", TotalChunks: 2}
	first, err := svc.InitiateUpload(context.Background(), req)
	require.NoError(t, err)

	second, err := svc.InitiateUpload(context.Background(), req)
	require.NoError(t, err, "重复 init 应幂等返回既有控制记录")
	assert.Equal(t, first.UploadID, second.UploadID)
	assert.Equal(t, first.ID, second.ID, "应返回同一条控制记录，而非新建")
}

// 测试：上传单个分片 → 标记完成
func TestUploadChunk_Success(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.InitiateUpload(context.Background(), &InitiateUploadReq{
		UploadID: "u1", TotalChunks: 2, FileName: "f",
	})
	require.NoError(t, err)

	resp, err := svc.UploadChunk(context.Background(), &UploadChunkReq{
		UploadID:   "u1",
		ChunkIndex: 0,
		ChunkHash:  "hash0",
		ChunkSize:  10,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, resp.ChunkIndex)
	assert.True(t, resp.Completed)
	assert.Equal(t, "completed", resp.Status)
}

// 测试：无效分片索引（越界）拒绝
func TestUploadChunk_InvalidIndex(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.InitiateUpload(context.Background(), &InitiateUploadReq{
		UploadID: "u2", TotalChunks: 2,
	})
	require.NoError(t, err)

	_, err = svc.UploadChunk(context.Background(), &UploadChunkReq{
		UploadID:   "u2",
		ChunkIndex: 5, // 越界（TotalChunks=2）
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid chunk index")
}

// 测试：上传分片到不存在的 UploadID 拒绝
func TestUploadChunk_UploadNotFound(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.UploadChunk(context.Background(), &UploadChunkReq{
		UploadID:   "ghost",
		ChunkIndex: 0,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// 测试：上传进度检查
func TestCheckUploadProgress(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.InitiateUpload(context.Background(), &InitiateUploadReq{
		UploadID: "u3", TotalChunks: 3, FileSize: 30,
	})
	require.NoError(t, err)

	// 上传 2/3 分片
	_, err = svc.UploadChunk(context.Background(), &UploadChunkReq{UploadID: "u3", ChunkIndex: 0})
	require.NoError(t, err)
	_, err = svc.UploadChunk(context.Background(), &UploadChunkReq{UploadID: "u3", ChunkIndex: 1})
	require.NoError(t, err)

	progress, err := svc.CheckUploadProgress(context.Background(), "u3")
	require.NoError(t, err)
	assert.Equal(t, 3, progress.TotalChunks)
	assert.Equal(t, int64(2), progress.CompletedChunks)
	assert.NotEqual(t, "completed", progress.Status) // 2/3 未全部完成
}

// 测试：获取已上传分片索引列表
func TestGetUploadedChunkIndexes(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.InitiateUpload(context.Background(), &InitiateUploadReq{
		UploadID: "u4", TotalChunks: 3,
	})
	require.NoError(t, err)
	_, _ = svc.UploadChunk(context.Background(), &UploadChunkReq{UploadID: "u4", ChunkIndex: 1})
	_, _ = svc.UploadChunk(context.Background(), &UploadChunkReq{UploadID: "u4", ChunkIndex: 2})

	indexes, err := svc.GetUploadedChunkIndexes(context.Background(), "u4")
	require.NoError(t, err)
	assert.Contains(t, indexes, 1)
	assert.Contains(t, indexes, 2)
	assert.NotContains(t, indexes, 0)
}

// =====================================================================
// 2026-10-08 攻击面加固：分片计划自洽校验 + 会话归属 fail-closed
// =====================================================================

func TestValidateChunkPlan(t *testing.T) {
	// 合法：恰为 ⌈size/chunk⌉
	assert.NoError(t, ValidateChunkPlan(30, 10, 3))
	assert.NoError(t, ValidateChunkPlan(31, 10, 4)) // 尾部不满片
	assert.NoError(t, ValidateChunkPlan(10, 10, 1))
	assert.NoError(t, ValidateChunkPlan(1, 10, 1))

	// 总数与 ⌈size/chunk⌉ 不符（虚高刷行 / 不足造僵尸会话）
	assert.Error(t, ValidateChunkPlan(30, 10, 2))
	assert.Error(t, ValidateChunkPlan(30, 10, 4))
	assert.Error(t, ValidateChunkPlan(30, 10, 0))
	assert.Error(t, ValidateChunkPlan(30, 10, -1))

	// 超硬上限
	assert.Error(t, ValidateChunkPlan(20001, 1, 20001))

	// 非法入参
	assert.Error(t, ValidateChunkPlan(0, 10, 1))
	assert.Error(t, ValidateChunkPlan(30, 0, 3))
}

// TestValidateChunkPlan_UpperBound MaxTotalChunks 上限内必须可通过（防把正常
// 大文件上传误杀：10000 片 × 4MB = 40GB）
func TestValidateChunkPlan_UpperBound(t *testing.T) {
	assert.NoError(t, ValidateChunkPlan(int64(MaxTotalChunks)*1024*1024, 1024*1024, MaxTotalChunks))
}

// TestOwnedByCaller_LegacyEmptyOwnerIP 空 OwnerIP 老会话归属收紧（2026-10-08）：
// 此前无条件放行（任何同网调用方可写分片/取消他人进行中会话）。
func TestOwnedByCaller_LegacyEmptyOwnerIP(t *testing.T) {
	uid := uint(7)
	legacy := &model.UploadChunk{UploadID: "legacy-1", OwnerIP: ""} // 匿名老数据

	// 无令牌无用户：一律拒绝（fail-closed）
	assert.False(t, OwnedByCaller(legacy, "1.2.3.4", "", nil))
	// 任何 IP 都不再天然通过
	assert.False(t, OwnedByCaller(legacy, "1.2.3.4", "", &uid))

	// 同登录用户可通过
	owner := &model.UploadChunk{UploadID: "legacy-2", OwnerIP: "", UserID: &uid}
	assert.True(t, OwnedByCaller(owner, "9.9.9.9", "", &uid))
	assert.False(t, OwnedByCaller(owner, "9.9.9.9", "", nil))
}

// TestOwnedByCaller_NormalPath 常规路径（有 OwnerIP）语义不回归
func TestOwnedByCaller_NormalPath(t *testing.T) {
	uid := uint(7)
	rec := &model.UploadChunk{UploadID: "u1", OwnerIP: "1.2.3.4", UserID: &uid}

	assert.True(t, OwnedByCaller(rec, "1.2.3.4", "", nil))                                       // IP 一致
	assert.True(t, OwnedByCaller(rec, "5.6.7.8", "", &uid))                                      // 同用户
	assert.False(t, OwnedByCaller(rec, "5.6.7.8", "", nil))                                      // 陌生者
	assert.True(t, OwnedByCaller(rec, "5.6.7.8", "tok", nil) == VerifySessionToken("u1", "tok")) // 令牌通道随密钥配置而定
}
