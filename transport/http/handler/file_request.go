// 寄件码/反向收件 HTTP 端点（P2）。
// 2026-10-09 IDL 化（idl/request.thrift）：链接管理四端点迁至
// core/gen/handler/request/request_service.go（gen/router/request 注册，
// /api/v1/user 组挂 JWT-only UserAuth）；本文件保留访客投递重管道
// （multipart 解析/匿名日配额/存储/传输日志/失败清理——包内私有助手依赖），
// 由 gen/handler/request.GuestUpload 桥接调用。
package handler

import (
	"context"
	"fmt"
	"github.com/pigeonbox/contracts/errcode"
	"mime/multipart"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	requestApp "github.com/pigeonbox/core/app/request"
	"github.com/pigeonbox/core/pkg/gate"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/pkg/resp"
	"github.com/pigeonbox/core/pkg/transfer"
)

var requestSvc *requestApp.Service

// SetRequestService 注入寄件码服务（bootstrap 调用）
func SetRequestService(s *requestApp.Service) { requestSvc = s }

func getRequestService() *requestApp.Service { return requestSvc }

// totalDeclaredSize 申报总字节数（配额预检用）
func totalDeclaredSize(files []*multipart.FileHeader) int64 {
	var total int64
	for _, f := range files {
		total += f.Size
	}
	return total
}

// GuestSubmitFiles 访客投递（POST /api/v1/request/:token/upload，multipart files[]）。
// 显式受邀上传：不走 upload.open_upload 总闸（与 Gokapi File Requests 同策略）。
func GuestSubmitFiles(ctx context.Context, c *app.RequestContext) {
	svc := getRequestService()
	if svc == nil || shareSvc == nil {
		c.JSON(consts.StatusInternalServerError, map[string]interface{}{"code": errcode.CodeInternal, "message": "service 未就绪"})
		return
	}
	token := c.Param("token")

	form, err := c.MultipartForm()
	if err != nil {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": errcode.CodeInvalidParam, "message": "解析 multipart 失败: " + err.Error()})
		return
	}
	files := form.File["files"]
	if len(files) == 0 || len(files) > 100 {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": errcode.CodeInvalidParam, "message": "files 需为 1-100 项"})
		return
	}

	// 预检链接（约束在 Submit 内还会强校验；此处 fail-fast 避免无效落盘）
	if _, err := svc.GetPublic(ctx, token); err != nil {
		c.JSON(consts.StatusNotFound, map[string]interface{}{"code": errcode.CodeNotFound, "message": err.Error()})
		return
	}

	guestIP := middleware.ClientIP(c)
	// 访客按 IP 计入匿名日配额（2026-10-05 审计 P2：此前投递通道绕过日配额，
	// 拿到链接者可无限制向属主配额灌文件）。闸门在落盘前 fail-fast。
	if err := gate.CheckAnonymousQuota(ctx, guestIP, totalDeclaredSize(files)); err != nil {
		resp.NewTypedError(c, err)
		return
	}
	st := manageStorage
	// 访客投递为明文上传，不跳过魔数校验（encrypted=false）
	stored, ok := saveUploadEntries(ctx, c, st, files, false)
	if !ok {
		return
	}

	shareResult, err := svc.Submit(ctx, token, stored, guestIP)
	if err != nil {
		cleanupStored(ctx, st, stored)
		c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": errcode.CodeInvalidParam, "message": err.Error()})
		return
	}

	transfer.Record(transfer.Entry{
		Operation: transfer.OpUpload, FileCodeID: shareResult.ID, Code: shareResult.Code,
		FileName: fmt.Sprintf("%d files (request)", len(stored)), FileSize: shareResult.Size,
		Username: middleware.UsernameFromContext(ctx), IP: guestIP,
	})

	c.JSON(consts.StatusOK, map[string]interface{}{
		"code":    0,
		"message": "投递成功",
		"data":    map[string]interface{}{"file_count": len(stored)},
	})
}
