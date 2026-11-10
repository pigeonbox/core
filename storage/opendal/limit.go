package opendal

import (
	"errors"
	"fmt"
	"io"
)

// MaxWholeReadBytes 「整读」（Read/GetFile）路径的硬上限（core#118）：50MiB。
//
// 锚定预览上限的默认值（preview/service.go：max_preview_size 默认 50MB）——
// 整读只服务「取内容」类消费：MCP 下载有 PB_MCP_MAX_FILE_SIZE（默认 6MB）前置
// 上限、文本分享内容在库（file_codes.text）不经存储整读、预览走流式 Reader +
// 自身上限；下载链路本就走流式。50MiB 覆盖全部合法整读消费。
//
// 刻意**不**做成配置：配置语义允许 0=不限（preview.MaxFileSize 的口径），而本
// 常量是 OOM 面的硬顶——远端后端（SFTP/FTP/OneDrive）整读还叠加网络缓冲，
// 多并发整读下内存峰值 ≈ 并发数 × 对象大小，必须与配置漂移解耦。
const MaxWholeReadBytes = 50 << 20

// ErrReadTooLarge 整读超限（core#118）。调用方用 errors.Is 判断；
// storage 包有同名转发（storage.ErrReadTooLarge）。
var ErrReadTooLarge = errors.New("storage: 对象超过整读上限")

// ReadAllLimited 限量整读（core#118）：最多整读 max 字节；恰好 max 字节放行，
// 超出（哪怕 1 字节）返回 ErrReadTooLarge——**显式报错而非静默截断**（截断会把
// 半个对象当完整内容交给上层：预览/取内容少半截、按残缺数据做完整性判定）。
//
// 实现：io.LimitReader(max+1) 多读 1 字节作超限哨兵（不先 Stat 再读——Stat 与
// 读之间对象可变，且部分驱动的 Stat 与读不同源）。
func ReadAllLimited(r io.Reader, max int64) ([]byte, error) {
	if max < 0 {
		max = 0
	}
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%w（上限 %d 字节）", ErrReadTooLarge, max)
	}
	return data, nil
}
