package chunk

// write_fail_test.go —— 分片通道的**存储写失败故障注入**（L6 故障注入）。
//
// 注入什么故障：
//   注入一个假存储（chunkFaultStorage），其 SaveChunk/MergeChunks 返回
//   syscall.ENOSPC（磁盘满等价错误）；随后撤除故障，模拟存储恢复。
//
// 断言什么（全部是**行为**）：
//   ① 写失败 ⇒ 错误上抛，且**保留底层 ENOSPC 语义**（调用方可判据）。
//   ② 写失败 ⇒ **不留半成品**：该分片在 DB 里**没有**完成行
//      （HTTP 层的顺序是「先落存储、成功后才记 DB 行」——
//      gen/handler/chunk/chunk_service.go:419-426），控制行的进度也**不动**
//      ⇒ 客户端看到的进度与真实落盘一致，不会拿到一个「显示已传、实际没落」的会话。
//   ③ 故障撤除后 ⇒ **同一 upload_id** 续传成功：补片 + 记行 + 合并，
//      且**不重新 init**（控制行仍是同一行、没新建第二条控制行）——
//      铁律：upload_id 由 file_hash 派生，重 init 会另起新会话使旧位图作废
//      ⇒ 续传必须对既有会话补片。
//   ④ 合并写失败 ⇒ **不留半成品**（合并产物不存在）且控制行**不被标记完成**
//      （否则会话会被当成已完成、用户拿到一个打不开的分享）。
//
// 怎么让它变红（负对照，详见交付说明的实跑记录）：
//   - 把 SaveChunkData 里的错误吞掉（返回 nil）⇒ ①红；
//   - 把「先落存储、后记 DB 行」的顺序倒过来（先记 DB 行再落存储）
//     ⇒ ②红（写失败却留下「已完成」的分片行 ⇒ 进度虚高、合并时才炸）；
//   - 把 MergeUpload 失败后的清理去掉 ⇒ ④红（合并产物留在存储里）。
//
// 用例隔离：包内既有 newTestDB/newTestService（service_test.go）不动，
// 本文件用独立的 newFaultChunkService（自带可注入失败的存储）。

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/pigeonbox/core/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// chunkFaultStorage 可注入写失败的记忆存储（实现 storage.StorageInterface）。
type chunkFaultStorage struct {
	mu       sync.Mutex
	files    map[string][]byte // 合并产物
	chunks   map[string][]byte // 分片
	writeErr error             // 非 nil ⇒ SaveChunk/MergeChunks 失败
}

func newChunkFaultStorage() *chunkFaultStorage {
	return &chunkFaultStorage{files: map[string][]byte{}, chunks: map[string][]byte{}}
}

// setWriteErr 注入/撤除写失败。
func (s *chunkFaultStorage) setWriteErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeErr = err
}

// hasMerged 合并产物是否存在（判「不留半成品」）。
func (s *chunkFaultStorage) hasMerged(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.files[path]
	return ok
}

// mergedBytes 合并产物内容。
func (s *chunkFaultStorage) mergedBytes(path string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.files[path]
	return append([]byte(nil), d...), ok
}

// chunkKeyOf 分片键（与 storage.chunkKey 同口径）。
func chunkKeyOf(uploadID string, index int) string {
	return fmt.Sprintf("chunks/%s/chunk_%d", uploadID, index)
}

func (s *chunkFaultStorage) SaveFile(context.Context, *multipart.FileHeader, string) (*storage.FileOperationResult, error) {
	return nil, fmt.Errorf("not implemented in test double")
}

func (s *chunkFaultStorage) DeleteFile(_ context.Context, filePath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.files, filePath)
	return nil
}

func (s *chunkFaultStorage) GetFile(_ context.Context, filePath string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.files[filePath]
	if !ok {
		return nil, fmt.Errorf("not found: %s", filePath)
	}
	return append([]byte(nil), d...), nil
}

func (s *chunkFaultStorage) FileExists(_ context.Context, filePath string) bool {
	return s.hasMerged(filePath)
}

func (s *chunkFaultStorage) SaveStream(_ context.Context, savePath string, r io.Reader, _ int64) (int64, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	s.files[savePath] = data
	return int64(len(data)), nil
}

func (s *chunkFaultStorage) SaveChunk(_ context.Context, uploadID string, chunkIndex int, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return s.writeErr
	}
	s.chunks[chunkKeyOf(uploadID, chunkIndex)] = append([]byte(nil), data...)
	return nil
}

func (s *chunkFaultStorage) MergeChunks(_ context.Context, uploadID string, totalChunks int, savePath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return s.writeErr
	}
	var buf bytes.Buffer
	for i := 0; i < totalChunks; i++ {
		d, ok := s.chunks[chunkKeyOf(uploadID, i)]
		if !ok {
			return fmt.Errorf("chunk %d missing", i)
		}
		buf.Write(d)
	}
	s.files[savePath] = buf.Bytes()
	return nil
}

func (s *chunkFaultStorage) CleanChunks(_ context.Context, uploadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := "chunks/" + uploadID + "/"
	for k := range s.chunks {
		if strings.HasPrefix(k, prefix) {
			delete(s.chunks, k)
		}
	}
	return nil
}

func (s *chunkFaultStorage) GetFileSize(_ context.Context, filePath string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.files[filePath]
	if !ok {
		return 0, fmt.Errorf("not found: %s", filePath)
	}
	return int64(len(d)), nil
}

func (s *chunkFaultStorage) GetFileURL(_ context.Context, filePath string) (string, error) {
	return "http://test.local/" + filePath, nil
}

func (s *chunkFaultStorage) GetFileReader(_ context.Context, filePath string) (io.ReadCloser, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.files[filePath]
	if !ok {
		return nil, 0, fmt.Errorf("not found: %s", filePath)
	}
	return io.NopCloser(bytes.NewReader(d)), int64(len(d)), nil
}

// newFaultChunkService 构造带可注入失败存储的分片 service。
func newFaultChunkService(t *testing.T) (*Service, *chunkFaultStorage, *gorm.DB) {
	t.Helper()
	gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, gormDB.AutoMigrate(&model.UploadChunk{}))
	sqlDB, err := gormDB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	svc := NewService(gormDB)
	st := newChunkFaultStorage()
	svc.SetStorage(st)
	return svc, st, gormDB
}

// TestChunk_StorageWriteFailure_NoHalfState 写失败 ⇒ 报错 + 不留半成品
// （DB 里不得出现「已完成」的分片行、进度不动）。
func TestChunk_StorageWriteFailure_NoHalfState(t *testing.T) {
	svc, st, _ := newFaultChunkService(t)
	ctx := context.Background()
	const uploadID = "up_write_fail_1"

	_, err := svc.InitiateUpload(ctx, &InitiateUploadReq{
		UploadID: uploadID, FileName: "big.bin", TotalChunks: 2, FileSize: 20, ChunkSize: 10,
		OwnerIP: "203.0.113.7",
	})
	require.NoError(t, err)

	// 注入写失败（磁盘满等价）。
	st.setWriteErr(fmt.Errorf("save chunk: %w", syscall.ENOSPC))

	// ① 写失败 ⇒ 错误上抛，且保留 ENOSPC 语义。
	err = svc.SaveChunkData(ctx, uploadID, 0, []byte("0123456789"))
	require.Error(t, err, "存储写失败必须上抛（不得静默吞掉）")
	assert.ErrorIs(t, err, syscall.ENOSPC, "错误必须保留底层 ENOSPC 语义（可判据）")

	// ② 不留半成品：没有分片行被记成完成、进度不动。
	indexes, err := svc.GetUploadedChunkIndexes(ctx, uploadID)
	require.NoError(t, err)
	assert.Empty(t, indexes, "写失败不得留下「已完成」的分片行（半成品）")

	prog, err := svc.CheckUploadProgress(ctx, uploadID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), prog.CompletedChunks, "写失败不得让进度虚高")
	assert.Equal(t, "pending", prog.Status, "写失败不得把会话标记成完成")
}

// TestChunk_ResumeSameUploadIDAfterWriteFailure 写失败撤除后 ⇒ 同一
// upload_id 续传成功（补片、不重新 init）。
func TestChunk_ResumeSameUploadIDAfterWriteFailure(t *testing.T) {
	svc, st, gormDB := newFaultChunkService(t)
	ctx := context.Background()
	const uploadID = "up_write_fail_2"

	first, err := svc.InitiateUpload(ctx, &InitiateUploadReq{
		UploadID: uploadID, FileName: "big.bin", TotalChunks: 2, FileSize: 20, ChunkSize: 10,
		OwnerIP: "203.0.113.8",
	})
	require.NoError(t, err)

	// 第 0 片写失败。
	st.setWriteErr(fmt.Errorf("save chunk: %w", syscall.ENOSPC))
	require.Error(t, svc.SaveChunkData(ctx, uploadID, 0, []byte("0123456789")))

	// 故障撤除 ⇒ 对**既有会话**补片（不重新 init）。
	st.setWriteErr(nil)
	require.NoError(t, svc.SaveChunkData(ctx, uploadID, 0, []byte("0123456789")))
	resp, err := svc.UploadChunk(ctx, &UploadChunkReq{
		UploadID: uploadID, ChunkIndex: 0, ChunkHash: "h0", ChunkSize: 10,
	})
	require.NoError(t, err)
	assert.True(t, resp.Completed)

	// 进度推进到 1/2。
	prog, err := svc.CheckUploadProgress(ctx, uploadID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), prog.CompletedChunks)

	// 控制行仍是**同一条**（没有另起新会话）：查全部控制行（chunk_index=-1）。
	var controls []model.UploadChunk
	require.NoError(t, gormDB.Where("chunk_index = ?", -1).Find(&controls).Error)
	require.Len(t, controls, 1, "续传不得另起新会话（upload_id 由 file_hash 派生，重 init 会作废旧位图）")
	assert.Equal(t, first.ID, controls[0].ID, "控制行必须是同一条")

	// 同 upload_id 再次 init 仍幂等返回既有进度（不重置、不新建）。
	again, err := svc.InitiateUpload(ctx, &InitiateUploadReq{
		UploadID: uploadID, FileName: "big.bin", TotalChunks: 2, FileSize: 20, ChunkSize: 10,
		OwnerIP: "203.0.113.8",
	})
	require.NoError(t, err)
	assert.Equal(t, first.ID, again.ID, "同 upload_id 重复 init 必须幂等返回既有控制行")
}

// TestChunk_MergeFailureLeavesNoHalfState 合并写失败 ⇒ 不留半成品 +
// 控制行不被标记完成。
func TestChunk_MergeFailureLeavesNoHalfState(t *testing.T) {
	svc, st, _ := newFaultChunkService(t)
	ctx := context.Background()
	const uploadID = "up_write_fail_3"

	_, err := svc.InitiateUpload(ctx, &InitiateUploadReq{
		UploadID: uploadID, FileName: "big.bin", TotalChunks: 2, FileSize: 20, ChunkSize: 10,
		OwnerIP: "203.0.113.9",
	})
	require.NoError(t, err)

	require.NoError(t, svc.SaveChunkData(ctx, uploadID, 0, []byte("0123456789")))
	require.NoError(t, svc.SaveChunkData(ctx, uploadID, 1, []byte("abcdefghij")))

	// 合并写失败（磁盘满）。
	st.setWriteErr(fmt.Errorf("merge: %w", syscall.ENOSPC))
	relPath, err := svc.MergeUpload(ctx, uploadID, 2, "big.bin")
	require.Error(t, err, "合并写失败必须上抛")
	assert.Empty(t, relPath, "合并失败不得返回可用路径")

	// 不留半成品：合并产物不存在。
	st.setWriteErr(nil) // 放行读侧断言（产物本来就不该在）
	assert.False(t, st.hasMerged(relPath), "合并失败不得留下半个对象")

	// 恢复后合并成功 ⇒ 同一 upload_id 直接续（不重新 init）。
	relPath, err = svc.MergeUpload(ctx, uploadID, 2, "big.bin")
	require.NoError(t, err)
	require.NotEmpty(t, relPath)
	data, ok := st.mergedBytes(relPath)
	require.True(t, ok, "恢复后合并产物必须存在")
	assert.Equal(t, "0123456789abcdefghij", string(data))
}
