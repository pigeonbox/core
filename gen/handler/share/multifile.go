// 多文件分享下载辅助（P0 多文件）：子文件单流 + zip 打包流。
// 鉴权与计次由调用方完成（authorizeDownload + chargePickup），这里只做读取与响应写出。
package share

import (
	"context"
	"fmt"
	"io"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/pkg/transfer"
	"github.com/pigeonbox/core/repo/db/model"
)

// streamChildFile 子文件单流下载（/share/download?file=<id>）。
// 归属校验由调用方在**扣次之前**完成（GetShareChild）——本函数只做读取与响应写出。
func streamChildFile(ctx context.Context, c *app.RequestContext, fc *model.FileCode, child *model.FileCodeFile, viewerIP string) {
	// 统一下传送流：本地后端 c.File 原生 Range；远端后端服务端区间流（206）
	// 或全量 200 回退（驱动不支持区间读时）。惰性开流：这里只做归属校验取
	// 子文件元数据，不预开全量读器（Range 请求会多一次无用打开）。
	name := child.DisplayName()
	streamFileDownload(ctx, c, child.FilePath, name,
		func() { logDownload(ctx, fc, fc.Code, name, child.Size, viewerIP) })
}

// writeShareZip 多文件 zip 打包流式下载（/share/download 多文件分享的默认形态）。
// rc 由调用方在扣次之前组装好（组装失败不消耗取件次数）。
func writeShareZip(ctx context.Context, c *app.RequestContext, fileCode *model.FileCode, rc io.ReadCloser, viewerIP string) {
	logDownload(ctx, fileCode, fileCode.Code, fileCode.Code+".zip", fileCode.Size, viewerIP)
	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.zip"`, fileCode.Code))
	c.Header("Cache-Control", "no-store")
	// 未知总长（-1）→ chunked 传输；读至 EOF 自动关闭
	c.SetBodyStream(newCloseOnEOFReader(rc), -1)
}

// logDownload 传输日志（下载，异步 fire-and-forget）
func logDownload(ctx context.Context, fc *model.FileCode, code, fileName string, size int64, viewerIP string) {
	var logUser *uint
	logUsername := ""
	if uid, ok := middleware.UserIDFromContext(ctx); ok {
		logUser = &uid
	}
	if n := middleware.UsernameFromContext(ctx); n != "" {
		logUsername = n
	}
	transfer.Record(transfer.Entry{
		Operation: transfer.OpDownload, FileCodeID: fc.ID, Code: code,
		FileName: fileName, FileSize: size,
		UserID: logUser, APIKeyID: apiKeyIDPtr(ctx),
		Username: logUsername, IP: viewerIP,
	})
}
