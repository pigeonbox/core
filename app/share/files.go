// files.go — 直传落盘与下载取流的存储收口（share 域存储接缝）。
//
// HTTP 适配层（gen handler）不得直连 storage：磁盘布局（UUID 名 + 日期目录）、
// 「本地后端可绝对路径直传」「s3 可预签名直下」的分支知识全部封在本文件，
// 适配层只拿到 FilePayload（读流或本地绝对路径）与最终路径/大小。
package share

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"time"

	"github.com/pigeonbox/core/pkg/security"
	"github.com/pigeonbox/core/pkg/utils"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/pigeonbox/core/storage"
)

// FilePayload 下载取流载荷。LocalAbs 非空表示本地后端，调用方可走绝对路径
// 直传（原生 Range/断点续传）；否则经 ReadCloser 流式中转（读至 EOF 关闭）。
type FilePayload struct {
	ReadCloser io.ReadCloser
	Size       int64
	LocalAbs   string
}

// ErrShareFileNotFound 分享或其子文件不存在（调用方统一 404 语义，防探测）。
var ErrShareFileNotFound = errors.New("share file not found")

// SaveUploadFile 直传文件落盘。originalFilename 仅用于扩展名推导，
// 磁盘名由 UploadObjectRelPath 统一生成；返回落盘路径、实际大小与存储层
// 计算的文件哈希（供秒传/去重，可能为空），调用方据此创建分享记录。
func (s *Service) SaveUploadFile(ctx context.Context, fh *multipart.FileHeader, originalFilename string) (string, int64, string, error) {
	st, err := s.storageClient()
	if err != nil {
		return "", 0, "", err
	}
	result, err := st.SaveFile(ctx, fh, utils.UploadObjectRelPath(originalFilename))
	if err != nil {
		return "", 0, "", err
	}
	return result.FilePath, result.FileSize, result.FileHash, nil
}

// OpenShareDownload 单文件下载取流（鉴权与扣次由调用方先行完成）。
func (s *Service) OpenShareDownload(ctx context.Context, filePath string) (*FilePayload, error) {
	st, err := s.storageClient()
	if err != nil {
		return nil, err
	}
	if concrete, ok := st.(*storage.StorageService); ok {
		if abs := concrete.LocalAbsPath(filePath); abs != "" {
			return &FilePayload{LocalAbs: abs}, nil
		}
	}
	rc, size, err := st.GetFileReader(ctx, filePath)
	if err != nil {
		return nil, err
	}
	return &FilePayload{ReadCloser: rc, Size: size}, nil
}

// OpenShareDownloadRange 区间打开单文件下载流（Range 下载/断点续传，
// 鉴权与扣次由调用方先行完成）。返回 (区间读器, 待传字节数, 对象总大小)。
// 后端不支持区间读时返回 storage/opendal.ErrRangeUnsupported，调用方回退
// OpenShareDownload 全量流（200）；start 越界返回 os.ErrNotExist 语义（416）。
func (s *Service) OpenShareDownloadRange(ctx context.Context, filePath string, start, length int64) (io.ReadCloser, int64, int64, error) {
	st, err := s.storageClient()
	if err != nil {
		return nil, 0, 0, err
	}
	concrete, ok := st.(*storage.StorageService)
	if !ok {
		return nil, 0, 0, fmt.Errorf("%w（非统一存储实例）", storage.ErrRangeUnsupported)
	}
	rc, total, err := concrete.GetFileReaderRange(ctx, filePath, start, length)
	if err != nil {
		return nil, 0, total, err
	}
	if length <= 0 || start+length > total {
		length = total - start
	}
	return rc, length, total, nil
}

// StatShareFile 获取分享文件总大小（Range 解析需先知 total）。
// 后端非统一存储实例时返回 storage.ErrRangeUnsupported（调用方回退全量）。
func (s *Service) StatShareFile(ctx context.Context, filePath string) (int64, error) {
	st, err := s.storageClient()
	if err != nil {
		return 0, err
	}
	concrete, ok := st.(*storage.StorageService)
	if !ok {
		return 0, storage.ErrRangeUnsupported
	}
	return concrete.StatFile(ctx, filePath)
}

// OpenChildDownload 多文件分享的子文件取流。不存在/不属于该分享返回
// ErrShareFileNotFound；读流失败原样返回（调用方区分 404 与 500）。
func (s *Service) OpenChildDownload(ctx context.Context, code string, fileID uint) (*model.FileCode, *model.FileCodeFile, *FilePayload, error) {
	fc, child, err := s.GetShareChild(ctx, code, fileID)
	if err != nil || fc == nil || child == nil {
		return nil, nil, nil, ErrShareFileNotFound
	}
	payload, err := s.OpenShareDownload(ctx, child.FilePath)
	if err != nil {
		return nil, nil, nil, err
	}
	return fc, child, payload, nil
}

// PresignDownloadURL s3 直下预签名 GET（download.s3_direct_download 开关的
// 执行段）。后端不支持预签名或签发失败返回 error，调用方回退服务端中转。
// fileName 非空时签入 response-content-disposition，直下 302 客户端据此命名。
func (s *Service) PresignDownloadURL(ctx context.Context, filePath string, ttl time.Duration, fileName ...string) (string, error) {
	st, err := s.storageClient()
	if err != nil {
		return "", err
	}
	concrete, ok := st.(*storage.StorageService)
	if !ok {
		return "", errors.New("presign not supported by current backend")
	}
	disposition := ""
	if len(fileName) > 0 && fileName[0] != "" {
		// RFC 6266/5987 统一收口（ASCII 兜底名防老客户端丢名）
		disposition = security.ContentDispositionAttachment(fileName[0])
	}
	return concrete.PresignGetURL(ctx, filePath, ttl, disposition)
}

// storageClient 取统一存储实例。未注入（测试/降级路径）时懒加载本地后端兜底，
// 参数与历史 handler 兜底一致（./data/uploads），保证行为不漂移。
func (s *Service) storageClient() (storage.StorageInterface, error) {
	if s.storage != nil {
		return s.storage, nil
	}
	s.fallbackOnce.Do(func() {
		s.fallbackStorage = storage.NewLocalFallback("./data/uploads", "http://localhost:12345")
	})
	return s.fallbackStorage, nil
}
