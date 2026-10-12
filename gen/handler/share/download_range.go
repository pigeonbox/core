// download_range.go — 远端后端的 Range 下载（断点续传/P3 下载续传）。
//
// 本地后端由 c.File 原生支持 Range/206；远端后端此前只能全量 200，
// 播放器拖动/断点续传类客户端无法工作。这里补齐服务端区间流：
// 驱动层（s3 原生 Range GET，fs 走 Seek）→ StorageService.GetFileReaderRange
// → share.OpenShareDownloadRange → 本文件的解析与 206 响应。
// 驱动不支持区间读（webdav/ftp 等）时优雅回退全量 200。
package share

import (
	"context"
	"errors"
	"fmt"

	"strconv"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/pigeonbox/core/pkg/security"
	"github.com/pigeonbox/core/storage"
)

// rangeParseResult Range 头解析结论
type rangeParseResult int

const (
	rangeNone           rangeParseResult = iota // 无/多区间/格式不识别 → 回退全量 200（RFC 7233 允许忽略）
	rangeOK                                     // 可满足 → 206
	rangeNotSatisfiable                         // 语法合法但越界（start>=size / bytes=-0 / 空文件）→ 416
)

// parseByteRange 解析单区间 Range 头（仅 bytes 单位、单区间；RFC 7233）。
// 支持三种形态：bytes=A-B（B 越界截断到末尾）、bytes=A-（到末尾）、bytes=-S（末 S 字节）。
// 多区间（含逗号）不做 multipart 拼装，按规范忽略回退 200。
func parseByteRange(header string, size int64) (start, length int64, result rangeParseResult) {
	const unit = "bytes="
	if !strings.HasPrefix(header, unit) {
		return 0, 0, rangeNone
	}
	spec := strings.TrimSpace(header[len(unit):])
	if spec == "" || strings.Contains(spec, ",") {
		return 0, 0, rangeNone
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, rangeNone
	}
	first, last := strings.TrimSpace(spec[:dash]), strings.TrimSpace(spec[dash+1:])

	if first == "" {
		// suffix 形态 bytes=-S：末 S 字节
		s, err := strconv.ParseInt(last, 10, 64)
		if err != nil || s < 0 {
			return 0, 0, rangeNone
		}
		if s == 0 || size == 0 {
			return 0, 0, rangeNotSatisfiable
		}
		if s > size {
			s = size
		}
		return size - s, s, rangeOK
	}

	a, err := strconv.ParseInt(first, 10, 64)
	if err != nil || a < 0 {
		return 0, 0, rangeNone
	}
	if a >= size {
		return 0, 0, rangeNotSatisfiable
	}
	if last == "" {
		// bytes=A-：A 到末尾
		return a, size - a, rangeOK
	}
	b, err := strconv.ParseInt(last, 10, 64)
	if err != nil || b < a {
		return 0, 0, rangeNone
	}
	if b >= size {
		b = size - 1
	}
	return a, b - a + 1, rangeOK
}

// streamFileDownload 单文件下载统一收口（本地/远端后端，支持 Range 206）。
// 惰性开流：Range 请求只做 1 次 Stat + 1 次区间 GET，不会预开全量读器；
// filePath 为存储相对路径；fileName 为下载呈现名；logTransfer 在确认开始
// 传输内容时调用一次（200/206 记一次下载；416 不计）。
func streamFileDownload(ctx context.Context, c *app.RequestContext, filePath, fileName string, logTransfer func()) {
	// RFC 6266/5987 统一收口(pkg/security):非 ASCII 名补 filename* + ASCII
	// 兜底名,老客户端(Android 9 等)不再丢成 download.bin
	disposition := security.ContentDispositionAttachment(fileName)
	// 远端后端：Range 分支先行（避免为 Range 请求多开一次全量读器）
	if rh := string(c.GetHeader("Range")); rh != "" {
		if total, serr := getShareService().StatShareFile(ctx, filePath); serr == nil {
			start, length, res := parseByteRange(rh, total)
			switch res {
			case rangeNotSatisfiable:
				c.Header("Content-Range", fmt.Sprintf("bytes */%d", total))
				c.JSON(consts.StatusRequestedRangeNotSatisfiable, map[string]interface{}{
					"code":    416,
					"message": "请求区间不满足",
				})
				return
			case rangeOK:
				rc, n, rtotal, rerr := getShareService().OpenShareDownloadRange(ctx, filePath, start, length)
				if rerr == nil {
					logTransfer()
					c.Header("Content-Type", "application/octet-stream")
					c.Header("Content-Disposition", disposition)
					c.Header("Accept-Ranges", "bytes")
					c.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+n-1, rtotal))
					c.Header("Content-Length", fmt.Sprintf("%d", n))
					c.SetStatusCode(consts.StatusPartialContent)
					// Hertz 延迟流式写出：读至 EOF 自动关闭（回归要点见 DownloadFile 注释）
					c.SetBodyStream(newCloseOnEOFReader(rc), int(n))
					return
				}
				if !errors.Is(rerr, storage.ErrRangeUnsupported) {
					c.JSON(consts.StatusInternalServerError, map[string]interface{}{
						"code":    500,
						"message": fmt.Sprintf("区间读取失败: %v", rerr),
					})
					return
				}
				// 驱动不支持区间读 → 落入下方全量 200
			}
		}
	}

	// 全量路径：本地后端绝对路径直传（c.File 原生 Range/断点续传）；远端全量流式 200
	payload, err := getShareService().OpenShareDownload(ctx, filePath)
	if err != nil {
		c.JSON(consts.StatusInternalServerError, map[string]interface{}{
			"code":    500,
			"message": fmt.Sprintf("获取文件失败: %v", err),
		})
		return
	}
	if payload.LocalAbs != "" {
		logTransfer()
		c.Header("Content-Type", "application/octet-stream")
		c.Header("Content-Disposition", disposition)
		c.File(payload.LocalAbs)
		return
	}
	logTransfer()
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", disposition)
	c.Header("Accept-Ranges", "bytes")
	c.Header("Content-Length", fmt.Sprintf("%d", payload.Size))
	c.SetBodyStream(newCloseOnEOFReader(payload.ReadCloser), int(payload.Size))
}
