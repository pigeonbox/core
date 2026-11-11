package storage

import (
	"context"
	"errors"
	"io"
	"testing"
)

// swapChunkLimit 临时改写 MaxChunkBytes（测试换桩；返回恢复函数）。
func swapChunkLimit(t *testing.T, v int64) func() {
	t.Helper()
	old := MaxChunkBytes
	MaxChunkBytes = v
	return func() { MaxChunkBytes = old }
}

// countingReadCloser 计数读端（core#118 limit_test 同款「有界消费」判据）。
type countingReadCloser struct {
	data  []byte
	off   int
	count int
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	r.count += n
	return n, nil
}

func (r *countingReadCloser) Close() error { return nil }

// TestSaveChunk_RejectsOversizeChunk（core#140 正判据）：SaveChunk 入库闸——
// 恰好上限放行，超 1 字节报 ErrChunkTooLarge（显式拒绝，不静默收下）。
//
// 负对照（改错转红）：去掉入库闸 ⇒ 超限用例转红（err==nil，超限分片入库）。
func TestSaveChunk_RejectsOversizeChunk(t *testing.T) {
	s := NewStorageService(&StorageConfig{Type: StorageTypeLocal, DataPath: t.TempDir()})
	restore := swapChunkLimit(t, 4)
	defer restore()

	if err := s.SaveChunk(context.Background(), "u", 0, []byte("1234")); err != nil {
		t.Fatalf("恰好上限应放行: %v", err)
	}
	err := s.SaveChunk(context.Background(), "u", 1, []byte("12345"))
	if !errors.Is(err, ErrChunkTooLarge) {
		t.Fatalf("超限分片必须报 ErrChunkTooLarge: %v", err)
	}
}

// TestMergeChunks_ChunkReadBounded（core#140 有界消费结构判据）：MergeChunks
// 本地分支逐片整读必须经 ReadAllLimited 限量——注入计数读端交付远超上限的字节：
//
//  1. 合并失败且 errors.Is(err, ErrReadTooLarge)（显式报错不静默截断——截断会
//     把半截分片当完整内容拼进合并产物）；
//  2. 读端实际消费 ≤ 上限+1（**有界消费**：内存保护的结构事实，独立于错误语义。
//     A5 教训：只把判超限改成事后检查、去掉 LimitReader 时错误照报但整读已发生，
//     必须有本条才测得出）。
//
// 负对照（改错转红，均已实跑）：
//   - 逐片整读回退 io.ReadAll（或 os.ReadFile）⇒ ② 转红（消费=全部 108 字节）；
//   - 去掉超限判定 ⇒ ① 转红。
func TestMergeChunks_ChunkReadBounded(t *testing.T) {
	s := NewStorageService(&StorageConfig{Type: StorageTypeLocal, DataPath: t.TempDir()})
	restore := swapChunkLimit(t, 8)
	defer restore()

	cr := &countingReadCloser{data: make([]byte, 108)}
	oldOpen := openChunkFile
	openChunkFile = func(string) (io.ReadCloser, error) { return cr, nil }
	defer func() { openChunkFile = oldOpen }()

	err := s.MergeChunks(context.Background(), "u", 1, "merged.bin")
	if err == nil || !errors.Is(err, ErrReadTooLarge) {
		t.Fatalf("超限分片必须显式报 ErrReadTooLarge: %v", err)
	}
	if cr.count > 9 {
		t.Fatalf("逐片整读必须有界消费（内存保护）：实际消费 %d 字节，期望 ≤ 9（上限 8 + 哨兵 1）", cr.count)
	}
}

// TestMergeChunks_LocalMergeWithinLimit 正向对照（判据不恒红）：限内分片经
// 限量整读正常合并、产物逐字节一致。
func TestMergeChunks_LocalMergeWithinLimit(t *testing.T) {
	s := NewStorageService(&StorageConfig{Type: StorageTypeLocal, DataPath: t.TempDir()})
	ctx := context.Background()
	for i, part := range []string{"aaa", "bbb", "ccc"} {
		if err := s.SaveChunk(ctx, "u2", i, []byte(part)); err != nil {
			t.Fatalf("保存分片 %d 失败: %v", i, err)
		}
	}
	if err := s.MergeChunks(ctx, "u2", 3, "merged2.bin"); err != nil {
		t.Fatalf("限内分片应正常合并: %v", err)
	}
	got, err := s.GetFile(ctx, "merged2.bin")
	if err != nil || string(got) != "aaabbbccc" {
		t.Fatalf("合并产物错误: %q err=%v", got, err)
	}
}
