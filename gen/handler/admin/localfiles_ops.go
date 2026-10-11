// gen/handler/admin/localfiles_ops.go — 管理端残留手写面 IDL 化收编
// （2026-10-10：local-files 三端点 / 设置测试端点 / 审计日志自 customHandler
// 与 routes_custom 内联闭包迁入，wire 形态逐字段保形）。
package admin

import (
	"context"
	"errors"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	admin "github.com/pigeonbox/contracts/gen/admin"
	shareService "github.com/pigeonbox/core/app/share"
	customHandler "github.com/pigeonbox/core/transport/http/handler"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/pkg/transfer"
	"github.com/pigeonbox/core/pkg/utils"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
)

// shareSvc 本地文件管理依赖（wiring 注入；与 customHandler 共用同一全站实例）。
var shareSvc *shareService.Service

// SetShareService 注入分享服务（bootstrap/wiring 调用）。
func SetShareService(s *shareService.Service) { shareSvc = s }

func getOpsShareService() *shareService.Service {
	if shareSvc == nil {
		// 兜底实例仅测试/降级路径可达：与 gen/handler/share 同策略。
		shareSvc = shareService.NewService("", nil)
	}
	return shareSvc
}

// AdminListLocalFiles 本地文件管理：白名单根目录+条目列表。
// @router /admin/local-files [GET]
func AdminListLocalFiles(ctx context.Context, c *app.RequestContext) {
	var req admin.AdminListLocalFilesReq
	_ = c.BindAndValidate(&req)
	entries, err := getOpsShareService().ListLocalFiles(int(req.GetRoot()), req.GetDir())
	if err != nil {
		c.JSON(consts.StatusBadRequest, &admin.AdminListLocalFilesResp{
			Code: 400, Message: err.Error(),
		})
		return
	}
	items := make([]*admin.LocalFileEntry, 0, len(entries))
	for _, e := range entries {
		items = append(items, &admin.LocalFileEntry{
			Name: e.Name, Path: e.Path, Size: e.Size,
			ModTime: e.ModTime.Format(time.RFC3339Nano), IsDir: e.IsDir,
		})
	}
	c.JSON(consts.StatusOK, &admin.AdminListLocalFilesResp{
		Code: 200, Message: "获取成功",
		Data: &admin.AdminListLocalFilesData{
			Roots:   shareService.LocalImportRoots(),
			Entries: items,
		},
	})
}

// AdminDeleteLocalFile 删除白名单目录内文件。
// @router /admin/local-files [DELETE]
func AdminDeleteLocalFile(ctx context.Context, c *app.RequestContext) {
	var req admin.AdminDeleteLocalFileReq
	_ = c.BindAndValidate(&req)
	if req.Path == "" {
		c.JSON(consts.StatusBadRequest, &admin.AdminDeleteLocalFileResp{
			Code: 400, Message: "缺少 path",
		})
		return
	}
	if err := getOpsShareService().DeleteLocalFile(int(req.GetRoot()), req.Path); err != nil {
		c.JSON(consts.StatusBadRequest, &admin.AdminDeleteLocalFileResp{
			Code: 400, Message: err.Error(),
		})
		return
	}
	// wire 增量：data 键不再下发（原恒 null）
	c.JSON(consts.StatusOK, &admin.AdminDeleteLocalFileResp{
		Code: 200, Message: "删除成功",
	})
}

// AdminImportLocalFile 把白名单目录内文件导入为分享（配额/审核同链路）。
// @router /admin/local-files/import [POST]
func AdminImportLocalFile(ctx context.Context, c *app.RequestContext) {
	var req admin.AdminImportLocalFileReq
	if err := c.BindAndValidate(&req); err != nil || req.Path == "" {
		c.JSON(consts.StatusBadRequest, &admin.AdminImportLocalFileResp{
			Code: 400, Message: "请求体无效（需 root+path）",
		})
		return
	}
	expireStyle := req.GetExpireStyle()
	expireValue := req.GetExpireValue()
	if expireStyle == "" {
		expireStyle = "day"
		if expireValue <= 0 {
			expireValue = 7
		}
	}
	if err := utils.CheckExpireStyleAllowed(expireStyle); err != nil {
		c.JSON(consts.StatusBadRequest, &admin.AdminImportLocalFileResp{
			Code: 400, Message: err.Error(),
		})
		return
	}
	passwordHash, err := utils.ResolveSharePassword(req.GetRequireAuth(), req.GetPassword())
	if err != nil {
		if errors.Is(err, utils.ErrPasswordRequired) {
			c.JSON(consts.StatusBadRequest, &admin.AdminImportLocalFileResp{
				Code: 400, Message: "开启密码保护时必须提供密码",
			})
			return
		}
		c.JSON(consts.StatusInternalServerError, &admin.AdminImportLocalFileResp{
			Code: 500, Message: "密码处理失败",
		})
		return
	}
	absPath, err := shareService.LocalImportAbsPath(int(req.GetRoot()), req.Path)
	if err != nil {
		c.JSON(consts.StatusBadRequest, &admin.AdminImportLocalFileResp{
			Code: 400, Message: err.Error(),
		})
		return
	}
	result, err := getOpsShareService().ImportLocalFile(ctx, shareService.ImportLocalOpts{
		AbsPath:      absPath,
		ExpireValue:  int(expireValue),
		ExpireStyle:  expireStyle,
		RequireAuth:  req.GetRequireAuth(),
		PasswordHash: passwordHash,
		CustomCode:   req.GetCustomCode(),
		OwnerIP:      middleware.ClientIP(c),
	})
	if err != nil {
		c.JSON(consts.StatusBadRequest, &admin.AdminImportLocalFileResp{
			Code: 400, Message: err.Error(),
		})
		return
	}

	transfer.Record(transfer.Entry{
		Operation: transfer.OpUpload, FileCodeID: result.ID, Code: result.Code,
		FileName: result.Text, FileSize: result.Size,
		Username: middleware.UsernameFromContext(ctx), IP: middleware.ClientIP(c),
	})

	c.JSON(consts.StatusOK, &admin.AdminImportLocalFileResp{
		Code: 200, Message: "导入成功",
		Data: &admin.AdminImportLocalFileData{
			Code: result.Code, ShareURL: result.FullShareURL,
		},
	})
}

// AdminTestSMTP 用当前生效 SMTP 配置发送测试邮件。
// @router /admin/notify/smtp/test [POST]
func AdminTestSMTP(ctx context.Context, c *app.RequestContext) {
	var req admin.AdminTestSMTPReq
	if err := c.BindAndValidate(&req); err != nil || req.To == "" {
		c.JSON(consts.StatusBadRequest, &admin.AdminTestSMTPResp{
			Code: 400, Message: "需要收件邮箱 to",
		})
		return
	}
	if err := customHandler.TestSMTPNow(req.To); err != nil {
		c.JSON(consts.StatusBadRequest, &admin.AdminTestSMTPResp{
			Code: 400, Message: err.Error(),
		})
		return
	}
	c.JSON(consts.StatusOK, &admin.AdminTestSMTPResp{
		Code: 200, Message: "测试邮件已发送",
	})
}

// AdminTestOIDC 验证当前生效 OIDC issuer discovery 可达。
// 从全局配置现构建 service（与热重建同源，「测的就是当前生效值」语义不变）。
// @router /admin/oidc/test [POST]
func AdminTestOIDC(ctx context.Context, c *app.RequestContext) {
	if err := customHandler.TestOIDCDiscovery(ctx); err != nil {
		c.JSON(consts.StatusBadRequest, &admin.AdminTestOIDCResp{
			Code: 400, Message: err.Error(),
		})
		return
	}
	c.JSON(consts.StatusOK, &admin.AdminTestOIDCResp{
		Code: 200, Message: "OIDC discovery 验证通过",
	})
}

// AdminActivities 管理操作审计日志分页。
// @router /admin/activities [GET]
func AdminActivities(ctx context.Context, c *app.RequestContext) {
	var req admin.AdminActivitiesReq
	_ = c.BindAndValidate(&req)
	page, pageSize := int(req.GetPage()), int(req.GetPageSize())
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 200 {
		pageSize = 200
	}
	query := model.AdminOperationLogQuery{
		Action:   req.GetAction(),
		Actor:    req.GetActor(),
		Page:     page,
		PageSize: pageSize,
	}
	if s := req.GetSuccess(); s == "true" || s == "false" {
		b := s == "true"
		query.Success = &b
	}
	logs, total, err := dao.NewAdminOperationLogRepository().List(ctx, query)
	if err != nil {
		c.JSON(consts.StatusOK, &admin.AdminActivitiesResp{
			Code: 50001, Message: "查询审计日志失败: " + err.Error(),
		})
		return
	}
	items := make([]*admin.AdminActivityItem, 0, len(logs))
	for _, lg := range logs {
		item := admin.AdminActivityItem{
			Action: lg.Action, Target: lg.Target, Success: lg.Success,
			Message: lg.Message, ActorName: lg.ActorName,
			IP: lg.IP, LatencyMs: lg.LatencyMs,
			CreatedAt: lg.CreatedAt.Format(time.RFC3339Nano),
		}
		if lg.ActorID != nil {
			id := int64(*lg.ActorID)
			item.ActorID = &id
		}
		items = append(items, &item)
	}
	c.JSON(consts.StatusOK, &admin.AdminActivitiesResp{
		Code: 200, Message: "获取成功",
		Data: &admin.AdminActivitiesData{
			Items: items, Total: total, Page: int32(page), PageSize: int32(pageSize),
		},
	})
}
