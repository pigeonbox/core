package share

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/repo/db/model"
	"go.uber.org/zap"
)

// zipStreamTimeout zip 组装整体超时（异步 goroutine 内的存储读取/写管路上限）。
// 大文件打包可能远超单请求超时，取宽松值防泄漏。
const zipStreamTimeout = 30 * time.Minute

// ZipStream 把分享的全部文件打包为 zip，返回读端流（未知总长，逐文件从存储拉取）。
//
// 实现要点：
//   - io.Pipe + goroutine 组装：handler 以 c.SetBodyStream(r, -1) 流式回写，
//     不落盘、不整包进内存（local/S3 统一经 StorageInterface.GetFileReader）
//   - 读端关闭即停写（管道背压）；组装出错时 CloseWithError 终止下载而非挂死
//   - zip 内文件名用展示名去路径（防 Zip Slip），重名自动追加序号
//   - context 用 WithoutCancel：handler 返回后请求 ctx 被取消，但流仍在被读取
//
// 分享无任何文件时返回错误（调用方转 404/500）。
func (s *Service) ZipStream(ctx context.Context, fc *model.FileCode) (io.ReadCloser, error) {
	items, err := s.shareFileItems(ctx, fc)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("分享内没有可下载的文件")
	}
	if len(items) == 1 {
		// 单文件（含 legacy 主表行）：直接回流原始文件，不打 zip 壳
		return s.singleFileStream(ctx, fc, items[0])
	}

	// 解析子表行（需要存储相对路径；legacy 虚拟行走主表路径）
	children, cerr := s.fileFileRepo.ListByFileCodeID(ctx, fc.ID)
	if cerr != nil {
		return nil, cerr
	}
	entries := make([]zipItem, 0, len(children))
	if len(children) > 0 {
		for _, c := range children {
			entries = append(entries, zipItem{
				name: zipSafeName(c.DisplayName(), usedNames(entries)),
				rel:  c.FilePath,
			})
		}
	} else if rel := fc.GetFilePath(); rel != "" {
		entries = append(entries, zipItem{
			name: zipSafeName(legacyFileNameOf(fc), usedNames(entries)),
			rel:  rel,
		})
	}

	pr, pw := io.Pipe()
	buildCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), zipStreamTimeout)
	go func() {
		defer cancel()
		zw := zip.NewWriter(pw)
		var buildErr error
		for _, e := range entries {
			rc, size, gerr := s.storage.GetFileReader(buildCtx, e.rel)
			if gerr != nil {
				buildErr = fmt.Errorf("读取文件 %s 失败: %w", e.name, gerr)
				break
			}
			hdr := &zip.FileHeader{
				Name:     e.name,
				Method:   zip.Deflate,
				Modified: time.Now(),
			}
			// 大文件免压缩收益低且耗 CPU：未知类型统一 Deflate，文本收益明显
			fw, cerr := zw.CreateHeader(hdr)
			if cerr == nil {
				_, cerr = io.CopyN(fw, rc, size) // size 已知：不依赖 EOF 语义（S3 读端 EOF 行为一致）
			}
			_ = rc.Close()
			if cerr != nil {
				buildErr = fmt.Errorf("打包文件 %s 失败: %w", e.name, cerr)
				break
			}
		}
		if zerr := zw.Close(); zerr != nil && buildErr == nil {
			buildErr = zerr
		}
		if buildErr != nil {
			logger.Warn("zip stream build failed", zap.String("code", fc.Code), zap.Error(buildErr))
			_ = pw.CloseWithError(buildErr)
			return
		}
		_ = pw.Close()
	}()
	return pr, nil
}

// singleFileStream 单文件流（legacy 虚拟行走主表路径）
func (s *Service) singleFileStream(ctx context.Context, fc *model.FileCode, item *ShareFileItem) (io.ReadCloser, error) {
	rel := fc.GetFilePath()
	if item.ID != 0 {
		if child, err := s.fileFileRepo.GetByID(ctx, item.ID); err == nil {
			rel = child.FilePath
		}
	}
	if rel == "" {
		return nil, fmt.Errorf("文件路径为空")
	}
	rc, _, err := s.storage.GetFileReader(ctx, rel)
	return rc, err
}

// zipItem zip 组装条目（name=zip 内展示名，rel=存储相对路径）
type zipItem struct {
	name string
	rel  string
}

// usedNames 提取已用 zip 名列表（zipSafeName 去重入参）
func usedNames(entries []zipItem) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.name)
	}
	return names
}

// zipSafeName zip 内展示名：去目录（防 Zip Slip）、去控制字符、重名追加序号。
// used 为已用名切片（追加去重名后由调用方保留）。
func zipSafeName(name string, used []string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	var b strings.Builder
	for _, r := range name {
		if r < 32 || r == 127 {
			continue
		}
		b.WriteRune(r)
	}
	name = b.String()
	if name == "" || name == "." || name == ".." {
		name = "file"
	}
	final := name
	for i := 1; ; i++ {
		taken := false
		for _, u := range used {
			if u == final {
				taken = true
				break
			}
		}
		if !taken {
			return final
		}
		ext := path.Ext(name)
		base := strings.TrimSuffix(name, ext)
		final = fmt.Sprintf("%s(%d)%s", base, i, ext)
	}
}
