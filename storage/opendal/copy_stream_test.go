package opendal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// sha256File 流式哈希（测试辅助，不整读）。
func sha256File(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TestCopyFS_StreamingBoundedMemory（core#140 正判据）：FS 拷贝必须**流式**。
// 用超过 MaxWholeReadBytes（50MiB）的 52MiB 源文件拷贝，两个判据：
//
//  1. 拷贝成功且产物逐字节一致（sha256 流式对比）——顺带锁「拷贝不受整读上限
//     封顶」：拷贝不是「取内容」消费，误套 ReadAllLimited/MaxWholeReadBytes
//     会砍掉合法大文件拷贝 ⇒ 本条转红；
//  2. **有界分配**（内存保护的结构事实，独立于结果正确性）：Copy 期间
//     TotalAlloc 增量 < 8MiB——os.ReadFile 整读 ≥ 52MiB ⇒ 回退即转红；
//     流式 io.Copy 只有固定缓冲。
//
// 负对照（改错转红，均已实跑）：
//   - 回退 os.ReadFile + os.WriteFile ⇒ ② 转红（分配 ≥ 52MiB）；
//   - 误套 ReadAllLimited(…, MaxWholeReadBytes) ⇒ ① 转红（52MiB 源超限报错）。
func TestCopyFS_StreamingBoundedMemory(t *testing.T) {
	const fileSize = 52 << 20 // > MaxWholeReadBytes（50MiB）
	root := t.TempDir()
	op, err := New(Config{Scheme: SchemeFS, Root: root})
	if err != nil {
		t.Fatal(err)
	}

	srcRel := "big.bin"
	dstRel := "copy/big.bin"
	srcFull := filepath.Join(root, srcRel)
	block := make([]byte, 1<<20)
	for i := range block {
		block[i] = byte(i * 7)
	}
	f, err := os.Create(srcFull)
	if err != nil {
		t.Fatal(err)
	}
	for written := 0; written < fileSize; written += len(block) {
		if _, err := f.Write(block); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if err := op.Copy(context.Background(), srcRel, dstRel); err != nil {
		t.Fatalf("拷贝不应受整读上限封顶、也不应失败: %v", err)
	}
	runtime.ReadMemStats(&after)
	alloc := after.TotalAlloc - before.TotalAlloc
	if alloc >= 8<<20 {
		t.Fatalf("FS 拷贝必须流式（有界分配）：Copy 期间分配 %d 字节，期望 < %d（os.ReadFile 整读形态 ≥ %d）",
			alloc, 8<<20, fileSize)
	}

	srcHash := sha256File(t, srcFull)
	dstHash := sha256File(t, filepath.Join(root, dstRel))
	if srcHash != dstHash {
		t.Fatalf("拷贝产物必须逐字节一致: src=%s dst=%s", srcHash, dstHash)
	}
}

// TestCopyFS_Basic 小文件拷贝正确性 + 覆盖既有产物（判据不恒红的最小正向对照）。
func TestCopyFS_Basic(t *testing.T) {
	root := t.TempDir()
	op, err := New(Config{Scheme: SchemeFS, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Write(context.Background(), "a.txt", []byte("hello copy")); err != nil {
		t.Fatal(err)
	}
	if err := op.Write(context.Background(), "b.txt", []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := op.Copy(context.Background(), "a.txt", "b.txt"); err != nil {
		t.Fatalf("拷贝失败: %v", err)
	}
	got, err := op.Read(context.Background(), "b.txt")
	if err != nil || string(got) != "hello copy" {
		t.Fatalf("拷贝产物错误: got=%q err=%v", got, err)
	}
}
