package storage

// fault_driver_test.go —— 存储写失败的 **故障注入**（L6 故障注入）。
//
// 注入什么故障：
//   假驱动（faultDriver）在 Write / WriteStream 上返回 **syscall.ENOSPC**
//   （磁盘满的等价错误），模拟「远端后端/磁盘写不进去」；
//   随后清掉故障，模拟「存储恢复」。
//
// 断言什么（全部是**行为**）：
//   ① 写失败 ⇒ 错误上抛（不是静默成功、不是「写了一半」）；
//   ② 写失败 ⇒ **不留半成品**：目标对象键在驱动里**不存在**
//      （半个包/半个对象不能被当成可用数据）。
//   ③ 写失败 ⇒ **分片不丢**：已落的分片仍在驱动里 ⇒ 客户端可以对**既有会话**
//      补片续传（铁律：upload_id 由 file_hash 派生，重 init 会另起新会话使旧
//      位图作废 ⇒ 续传必须补片，不能重新 init）。
//   ④ 故障撤除后 ⇒ **同一 upload_id** 的合并成功，内容逐字正确
//      （证明续传路径没被写失败打断、也没留下脏状态）。
//
// 怎么让它变红（负对照，详见交付说明的实跑记录）：
//   - 把 faultDriver.WriteStream 改成「先写一半再报错」⇒ ②红
//     （合并产物会留在驱动里 ⇒ 半成品被当成可用数据）；
//   - 把 MergeChunks 的失败分支改成「顺手清掉分片」⇒ ③红
//     （续传必需的分片被删了 ⇒ 同一 upload_id 无法补片，只能重传）；
//   - 把 SaveChunk 的失败改成吞掉错误（返回 nil）⇒ ①红。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/pigeonbox/core/storage/opendal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// faultDriver 可注入失败的假驱动（测试专用）。
//
// 与既有 fakeDriver（storage_remote_test.go）的差别：**多一组可注入的失败**，
// 且写失败时**绝不落数据**（模拟真实后端的原子写语义：写失败 ⇒ 对象不存在）。
type faultDriver struct {
	mu    sync.Mutex
	data  map[string][]byte
	calls []string

	// writeErr 非 nil ⇒ Write/WriteStream 一律失败（ENOSPC 等价）。
	writeErr error
	// statErr 非 nil ⇒ Stat 一律失败（后端不可达）。
	statErr error
}

func newFaultDriver() *faultDriver { return &faultDriver{data: map[string][]byte{}} }

// setWriteErr 注入/撤除写失败。
func (f *faultDriver) setWriteErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeErr = err
}

// setStatErr 注入/撤除 Stat 失败。
func (f *faultDriver) setStatErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statErr = err
}

// get 带锁读取（并发安全：MergeChunks 会异步触发 CleanChunks goroutine）。
func (f *faultDriver) get(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.data[key]
	return append([]byte(nil), d...), ok
}

// keys 带锁列出全部键（判「不留半成品」用）。
func (f *faultDriver) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.data))
	for k := range f.data {
		out = append(out, k)
	}
	return out
}

func (f *faultDriver) record(call string) { f.calls = append(f.calls, call) }

func (f *faultDriver) Write(_ context.Context, key string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Write:" + key)
	if f.writeErr != nil {
		return f.writeErr
	}
	f.data[key] = append([]byte(nil), data...)
	return nil
}

func (f *faultDriver) WriteStream(_ context.Context, key string, r io.Reader, size int64) error {
	// 先在锁外读完（chain reader 会回调 Reader/Stat 再抢同一把锁，Mutex 不可重入）
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return fmt.Errorf("size mismatch: got %d want %d", len(data), size)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(fmt.Sprintf("WriteStream:%s:%d", key, size))
	if f.writeErr != nil {
		return f.writeErr
	}
	f.data[key] = data
	return nil
}

func (f *faultDriver) Read(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Read:" + key)
	if f.statErr != nil {
		return nil, f.statErr // 后端不可达 ⇒ 读失败（不得退化成「不存在」）
	}
	if d, ok := f.data[key]; ok {
		return append([]byte(nil), d...), nil
	}
	return nil, fmt.Errorf("%w: %s", os.ErrNotExist, key)
}

func (f *faultDriver) Reader(_ context.Context, key string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Reader:" + key)
	if f.statErr != nil {
		return nil, f.statErr // 后端不可达 ⇒ 读失败（不得退化成「不存在」）
	}
	if d, ok := f.data[key]; ok {
		return io.NopCloser(bytes.NewReader(d)), nil
	}
	return nil, fmt.Errorf("%w: %s", os.ErrNotExist, key)
}

func (f *faultDriver) Stat(_ context.Context, key string) (*opendal.Metadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Stat:" + key)
	if f.statErr != nil {
		return nil, f.statErr
	}
	if d, ok := f.data[key]; ok {
		return &opendal.Metadata{Path: key, Size: int64(len(d))}, nil
	}
	return nil, fmt.Errorf("%w: %s", os.ErrNotExist, key)
}

func (f *faultDriver) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("Delete:" + key)
	delete(f.data, key)
	return nil
}

func (f *faultDriver) RemoveAll(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("RemoveAll:" + key)
	prefix := key + "/"
	for k := range f.data {
		if k == key || strings.HasPrefix(k, prefix) {
			delete(f.data, k)
		}
	}
	return nil
}

// newFaultStorageService 构造走假驱动的远端存储服务（与既有
// newRemoteTestService 同款装配，只是驱动换成了可注入失败的那只）。
func newFaultStorageService(fd *faultDriver) *StorageService {
	return &StorageService{
		config: &StorageConfig{Type: StorageTypeS3, DataPath: "unused-remote", BaseURL: "http://x"},
		op:     opendal.NewCustom(opendal.SchemeS3, fd),
	}
}

// TestStorage_ChunkWriteFailure_NoPartialObject 分片写失败（ENOSPC）：
// 错误上抛 + 目标对象不存在 + 已落分片不丢（可续传）。
func TestStorage_ChunkWriteFailure_NoPartialObject(t *testing.T) {
	fd := newFaultDriver()
	svc := newFaultStorageService(fd)
	ctx := context.Background()
	const uploadID = "up_fault_1"

	// 先让一片成功落下（模拟「已传了第 0 片」）。
	require.NoError(t, svc.SaveChunk(ctx, uploadID, 0, []byte("part-0")))

	// 注入写失败（磁盘满等价）。
	fd.setWriteErr(fmt.Errorf("write chunks: %w", syscall.ENOSPC))

	// ① 写失败必须上抛（不是静默成功）。
	err := svc.SaveChunk(ctx, uploadID, 1, []byte("part-1"))
	require.Error(t, err, "分片写失败必须上抛（不得静默吞掉）")
	assert.ErrorIs(t, err, syscall.ENOSPC, "错误必须保留底层 ENOSPC 语义（可判据）")

	// ② 不留半成品：失败的那一片不得出现在存储里。
	_, ok := fd.get("chunks/" + uploadID + "/chunk_1")
	assert.False(t, ok, "写失败的分片不得留在存储里（半成品）")

	// ③ 已落的分片不丢 ⇒ 客户端可以对既有会话补片续传。
	data, ok := fd.get("chunks/" + uploadID + "/chunk_0")
	require.True(t, ok, "写失败不得牵连已落的分片（续传要靠它）")
	assert.Equal(t, []byte("part-0"), data)

	// ④ 撤除故障后，同一 upload_id 补片 + 合并成功（不重新 init）。
	fd.setWriteErr(nil)
	require.NoError(t, svc.SaveChunk(ctx, uploadID, 1, []byte("part-1")))

	const relPath = "uploads/2026/11/18/fault.bin"
	require.NoError(t, svc.MergeChunks(ctx, uploadID, 2, relPath))
	merged, ok := fd.get(relPath)
	require.True(t, ok, "恢复后合并产物必须存在")
	assert.Equal(t, []byte("part-0part-1"), merged, "合并内容必须逐字正确")
}

// TestStorage_MergeFailureLeavesNoObject 合并写失败（ENOSPC）：
// 合并产物**不得**留在存储里（半个对象不能被当成可用数据），
// 且分片仍在（可续传）。
func TestStorage_MergeFailureLeavesNoObject(t *testing.T) {
	fd := newFaultDriver()
	svc := newFaultStorageService(fd)
	ctx := context.Background()
	const uploadID = "up_fault_2"
	const relPath = "uploads/2026/11/18/half.bin"

	require.NoError(t, svc.SaveChunk(ctx, uploadID, 0, []byte("aaaa")))
	require.NoError(t, svc.SaveChunk(ctx, uploadID, 1, []byte("bbbb")))

	fd.setWriteErr(fmt.Errorf("merge: %w", syscall.ENOSPC))
	err := svc.MergeChunks(ctx, uploadID, 2, relPath)
	require.Error(t, err, "合并写失败必须上抛")
	assert.ErrorIs(t, err, syscall.ENOSPC)

	// 不留半成品：合并产物不存在。
	_, ok := fd.get(relPath)
	assert.False(t, ok, "合并失败不得留下半个对象（会被当成可用数据）")

	// 分片仍在 ⇒ 同一 upload_id 可以补片/重试合并。
	for i := 0; i < 2; i++ {
		_, ok := fd.get(fmt.Sprintf("chunks/%s/chunk_%d", uploadID, i))
		assert.True(t, ok, "合并失败不得清掉分片（续传要靠它）")
	}

	// 恢复后重试合并（同一 upload_id，不重新 init）⇒ 成功且内容正确。
	fd.setWriteErr(nil)
	require.NoError(t, svc.MergeChunks(ctx, uploadID, 2, relPath))
	merged, ok := fd.get(relPath)
	require.True(t, ok)
	assert.Equal(t, []byte("aaaabbbb"), merged)
}

// TestStorage_BackendDown_DoesNotDisguiseAsNotFound 后端不可达（Stat 失败）
// 时，读路径的错误**不得伪装成「对象不存在」**。
//
// 为什么：存储层把「对象不存在」与「后端挂了」混成同一个错误，调用方
// （取件面、备份面）就会把一次基础设施故障当成「文件没了」——取件会报
// 「分享不存在/已过期」，备份会把对象记成 missing 悄悄漏掉。
// 本仓既有纪律见 app/share 的 ErrShareGone（基建故障不得命中「码不存在」）。
//
// 负对照：把驱动的错误统一换成 os.ErrNotExist ⇒ 本用例转红。
func TestStorage_BackendDown_DoesNotDisguiseAsNotFound(t *testing.T) {
	fd := newFaultDriver()
	svc := newFaultStorageService(fd)
	ctx := context.Background()

	const uploadID = "up_fault_3"
	const objKey = "chunks/" + uploadID + "/chunk_0"
	require.NoError(t, svc.SaveChunk(ctx, uploadID, 0, []byte("x")))

	// 后端不可达。
	backendDown := errors.New("backend unavailable: 503 Service Unavailable")
	fd.setStatErr(backendDown)

	_, err := svc.GetFile(ctx, objKey)
	require.Error(t, err, "后端不可达时读必须失败")
	assert.NotErrorIs(t, err, os.ErrNotExist,
		"后端故障不得伪装成「对象不存在」（取件会误报分享没了、备份会漏对象）")

	_, err = svc.GetFileSize(ctx, objKey)
	require.Error(t, err)
	assert.NotErrorIs(t, err, os.ErrNotExist,
		"后端故障不得伪装成「对象不存在」")
}
