package storage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/pigeonbox/core/storage/opendal"
)

// TestGetFileLocalRejectsOversize（core#118 正判据）：整读超限对象 ⇒ 显式报
// ErrReadTooLarge（本地分支 os.ReadFile 整读收编为限量整读）。
//
// 负对照（改错转红）：把 GetFile 本地分支改回 os.ReadFile（或去掉
// opendal.ReadAllLimited 的 LimitReader 哨兵）⇒ 本用例转红（err==nil，超限
// 对象整读进内存返回）。
func TestGetFileLocalRejectsOversize(t *testing.T) {
	s := NewStorageService(&StorageConfig{Type: StorageTypeLocal, DataPath: t.TempDir()})

	// 写一个超过整读上限的对象（MaxWholeReadBytes+1，分块写避免测试自身大分配）
	if err := s.SaveBytes(context.Background(), "uploads/oversize.bin", nil); err != nil {
		t.Fatalf("预创建失败: %v", err)
	}
	full, err := s.localContain("uploads/oversize.bin")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(full)
	if err != nil {
		t.Fatal(err)
	}
	block := bytes.Repeat([]byte("x"), 1<<20)
	written := 0
	for int64(written) <= opendal.MaxWholeReadBytes {
		n, werr := f.Write(block)
		if werr != nil {
			t.Fatalf("写测试对象失败: %v", werr)
		}
		written += n
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := s.GetFile(context.Background(), "uploads/oversize.bin"); !errors.Is(err, ErrReadTooLarge) {
		t.Fatalf("超限对象必须报 ErrReadTooLarge: %v", err)
	}

	// 对照：限内对象正常返回（判据不是恒红）
	if err := s.SaveBytes(context.Background(), "uploads/ok.txt", []byte("ok")); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetFile(context.Background(), "uploads/ok.txt")
	if err != nil || string(got) != "ok" {
		t.Fatalf("限内对象应正常返回: got=%q err=%v", got, err)
	}
}
