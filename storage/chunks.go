// chunks.go 分片上传：chunk key、保存/合并/清理，与远端合并用的链式顺序读器。
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/oekit/concurrency/async"
	"github.com/pigeonbox/core/storage/opendal"
)

// MaxChunkBytes 单分片字节硬上限（core#140，core#118 族补界）：50MiB，与
// opendal.MaxWholeReadBytes 同锚点同值（整读硬顶）。此前单片字节数**无真界**：
// 实际分片大小从不与申报计划对账（UploadChunk 只记录 file.Size），唯一偶然是
// Hertz 全局请求体上限 max(upload.upload_size, upload.max_file_size)——随
// upload.max_file_size 配置漂移（配 10GB 就是 10GB/片）。本上限是 OOM 面硬顶，
// 刻意不做配置（与 core#118 同理由：配置语义允许 0=不限，硬顶必须与配置漂移
// 解耦）。三处同尺生效：handler 读入内存前拒（app/chunk.ValidateChunkBytes）、
// SaveChunk 入库闸、MergeChunks 逐片有界整读。
//
// ⚠️ 变量而非常量：测试换桩留口（有界消费判据需小上限 + 计数读端；参照
// federation randSource / outbound defaultObserver 先例）；生产恒为 50MiB，
// 运行期不改写。
var MaxChunkBytes int64 = opendal.MaxWholeReadBytes

// ErrChunkTooLarge 单分片字节超限（SaveChunk 入库闸；调用方 errors.Is 判断）。
var ErrChunkTooLarge = errors.New("storage: 分片超过单片字节上限")

// openChunkFile 分片文件打开口（测试换桩留口：MergeChunks 逐片有界消费判据需
// 注入计数读端；生产恒为 os.Open）。
var openChunkFile = func(path string) (io.ReadCloser, error) { return os.Open(path) }

// chunkKey 分片对象 key（fs/远端统一 '/' 分隔；fs 侧 filepath.Join 兼容处理）
func chunkKey(uploadID string, index int) string {
	return "chunks/" + uploadID + fmt.Sprintf("/chunk_%d", index)
}

// SaveChunk 保存分片。
//
// 入库闸（core#140）：单片字节数超 MaxChunkBytes 显式拒绝——storage 层是分片
// 尺寸契约的最终落点（handler 侧 app/chunk.ValidateChunkBytes 已在读入内存前
// 拒，此处兜底其他调用方与未来路径），保证 MergeChunks 的逐片整读上限永远不会
// 被合法入库数据触到。
func (s *StorageService) SaveChunk(ctx context.Context, uploadID string, chunkIndex int, data []byte) error {
	if int64(len(data)) > MaxChunkBytes {
		return fmt.Errorf("%w（%d 字节）", ErrChunkTooLarge, MaxChunkBytes)
	}
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
		// 逐片限量整读（core#140，收编 core#118 的 ReadAllLimited）：原
		// os.ReadFile 无界整读——单片可随全局请求体上限漂到 GB 级（见
		// MaxChunkBytes 注释）。超限显式报错不静默截断（截断会把半截分片
		// 当完整内容拼进合并产物）。
		cf, err := openChunkFile(chunkPath)
		if err != nil {
			return fmt.Errorf("读取分片 %d 失败: %w", i, err)
		}
		chunkData, rerr := opendal.ReadAllLimited(cf, MaxChunkBytes)
		_ = cf.Close()
		if rerr != nil {
			return fmt.Errorf("读取分片 %d 失败: %w", i, rerr)
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
