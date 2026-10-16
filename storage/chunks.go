// chunks.go 分片上传：chunk key、保存/合并/清理，与远端合并用的链式顺序读器。
package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/pigeonbox/core/storage/opendal"
	"github.com/oekit/concurrency/async"
)

// chunkKey 分片对象 key（fs/远端统一 '/' 分隔；fs 侧 filepath.Join 兼容处理）
func chunkKey(uploadID string, index int) string {
	return "chunks/" + uploadID + fmt.Sprintf("/chunk_%d", index)
}

// SaveChunk 保存分片
func (s *StorageService) SaveChunk(ctx context.Context, uploadID string, chunkIndex int, data []byte) error {
	_, op := s.current()
	key := chunkKey(uploadID, chunkIndex)
	if op != nil {
		return op.Write(ctx, key, data)
	}
	chunkPath, err := s.localContain(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(chunkPath), 0755); err != nil {
		return err
	}
	return os.WriteFile(chunkPath, data, 0644)
}

// MergeChunks 合并分片。
// local：顺序拼接落盘；远端：懒打开分片的链式读器 + 按总大小流式上传，
// 不整文件进内存。
func (s *StorageService) MergeChunks(ctx context.Context, uploadID string, totalChunks int, savePath string) error {
	_, op := s.current()

	if op != nil {
		total := int64(0)
		for i := 0; i < totalChunks; i++ {
			md, err := op.Stat(ctx, chunkKey(uploadID, i))
			if err != nil {
				return fmt.Errorf("定位分片 %d 失败: %w", i, err)
			}
			total += md.Size
		}
		reader := &chunkChainReader{ctx: ctx, op: op, uploadID: uploadID, n: totalChunks}
		defer func() { _ = reader.Close() }()
		if err := op.WriteStream(ctx, savePath, reader, total); err != nil {
			return fmt.Errorf("合并分片失败: %w", err)
		}
		// fire-and-forget 清理走 OpenDAL 驱动，GoSafe 兜底防驱动 panic 带崩进程
		async.GoSafe(func() { _ = s.CleanChunks(context.Background(), uploadID) })
		return nil
	}

	fullPath, err := s.localContain(savePath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return err
	}
	dst, err := os.Create(fullPath)
	if err != nil {
		return err
	}
	defer func() { _ = dst.Close() }()

	for i := 0; i < totalChunks; i++ {
		chunkPath, err := s.localContain(chunkKey(uploadID, i))
		if err != nil {
			return fmt.Errorf("读取分片 %d 失败: %w", i, err)
		}
		chunkData, err := os.ReadFile(chunkPath)
		if err != nil {
			return fmt.Errorf("读取分片 %d 失败: %w", i, err)
		}
		if _, err := dst.Write(chunkData); err != nil {
			return fmt.Errorf("写入分片 %d 失败: %w", i, err)
		}
	}

	// 同上：本地路径清理虽是纯 fs 操作，与远端路径保持同构兜底
	async.GoSafe(func() { _ = s.CleanChunks(context.Background(), uploadID) })
	return nil
}

// CleanChunks 清理分片
func (s *StorageService) CleanChunks(ctx context.Context, uploadID string) error {
	_, op := s.current()
	prefix := "chunks/" + uploadID
	if op != nil {
		return op.RemoveAll(ctx, prefix)
	}
	fullPath, err := s.localContain(prefix)
	if err != nil {
		return err
	}
	return os.RemoveAll(fullPath)
}

// chunkChainReader 远端合并分片用的顺序读器：按 chunk_0..chunk_{n-1} 懒打开，
// 读完一片自动切下一片，避免整文件进内存。
type chunkChainReader struct {
	ctx      context.Context
	op       *opendal.Operator
	uploadID string
	i, n     int
	cur      io.ReadCloser
}

func (r *chunkChainReader) Read(p []byte) (int, error) {
	for {
		if r.cur == nil {
			if r.i >= r.n {
				return 0, io.EOF
			}
			rc, err := r.op.Reader(r.ctx, chunkKey(r.uploadID, r.i))
			if err != nil {
				return 0, fmt.Errorf("读取分片 %d 失败: %w", r.i, err)
			}
			r.cur = rc
		}
		n, err := r.cur.Read(p)
		if err == io.EOF {
			// Read 允许数据与 EOF 同帧返回（(n>0, io.EOF)）：必须先交付这批
			// 字节、下轮 Read 再切下一片。此前直接丢弃 n，合并产物在
			// 「末段+EOF 同帧」场景静默缺尾（COS 实测 100000 字节分片只合
			// 并出 98304，Put 报 Body length 与 ContentLength 不符）。
			if n > 0 {
				return n, nil
			}
			_ = r.cur.Close()
			r.cur = nil
			r.i++
			continue
		}
		if err != nil {
			return n, err
		}
		if n > 0 {
			return n, nil
		}
	}
}

func (r *chunkChainReader) Close() error {
	if r.cur != nil {
		_ = r.cur.Close()
		r.cur = nil
	}
	return nil
}
