// gen/handler/p2p/p2p_service.go — 设备直传管理面（P2a，2026-10-14 对称节点
// spec §7/§8）。hz 首生成桩后本文件转手写（模型引 contracts/gen/p2p，
// hzmodel 临时目录不落地）。全部端点挂 /admin 组（AdminMiddleware），操作
// 留审计；数据面=supervisor spawn p2pc 子进程，本层只做编排，不代理文件流量。
package p2p

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	p2p "github.com/pigeonbox/contracts/gen/p2p"
	"github.com/pigeonbox/core/app/p2px"
	shareService "github.com/pigeonbox/core/app/share"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/pkg/utils"
)

// 装配注入（wiring 调用；与 admin gen handler 同款 fail-fast 模式）。
var (
	p2pxSvc  *p2px.Service
	shareSvc *shareService.Service
)

// SetService 注入直传 supervisor（bootstrap/wiring 调用）。
func SetService(s *p2px.Service) { p2pxSvc = s }

// SetShareService 注入分享服务（发布为分享复用本地导入铸链）。
func SetShareService(s *shareService.Service) { shareSvc = s }

func getP2px() *p2px.Service {
	if p2pxSvc == nil {
		panic("p2px service not initialized: SetService must be called before serving")
	}
	return p2pxSvc
}

func getShare() *shareService.Service {
	if shareSvc == nil {
		panic("share service not initialized: SetShareService must be called before serving")
	}
	return shareSvc
}

// ---- 工具 ----

func taskToModel(t *p2px.TaskSnapshot) *p2p.P2pTask {
	m := &p2p.P2pTask{
		ID:        t.ID,
		Kind:      t.Kind,
		State:     t.State,
		Code:      t.Code,
		Paths:     t.Paths,
		BytesDone: t.BytesDone,
		Error:     t.Error,
		CreatedAt: t.CreatedAt.Format(time.RFC3339),
	}
	if !t.FinishedAt.IsZero() {
		m.FinishedAt = t.FinishedAt.Format(time.RFC3339)
	}
	return m
}

func badReq(c *app.RequestContext, msg string) {
	c.JSON(consts.StatusBadRequest, map[string]any{"code": 400, "message": msg})
}

func okData(c *app.RequestContext, data map[string]any) {
	c.JSON(consts.StatusOK, map[string]any{"code": 200, "message": "success", "data": data})
}

// 审计（与 admin 域同表 admin_operation_logs；wiring 注入写者，nil=跳过）。
var auditWriter func(ctx context.Context, action, target string, success bool)

// SetAuditWriter 注入审计写者（wiring 装配）。
func SetAuditWriter(fn func(ctx context.Context, action, target string, success bool)) {
	auditWriter = fn
}

func audit(ctx context.Context, action, target string, err error) {
	if auditWriter != nil {
		auditWriter(ctx, action, target, err == nil)
	}
}

// ---- 端点 ----

// AdminP2pStatus 状态面。
// @router /admin/p2p/status [GET]
func AdminP2pStatus(ctx context.Context, c *app.RequestContext) {
	st, err := getP2px().Status(ctx)
	if err != nil {
		badReq(c, err.Error())
		return
	}
	okData(c, map[string]any{
		"available":      st.Available,
		"binary_path":    st.BinaryPath,
		"enabled":        st.Enabled,
		"registry_url":   st.RegistryURL,
		"recv_dir":       st.RecvDir,
		"inbox_used":     st.InboxUsed,
		"inbox_quota":    st.InboxQuota,
		"deploy_allowed": st.DeployAllowed,
	})
}

// AdminP2pTasks 任务列表。
// @router /admin/p2p/tasks [GET]
func AdminP2pTasks(ctx context.Context, c *app.RequestContext) {
	tasks := getP2px().List(ctx)
	items := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		items = append(items, map[string]any{
			"id":          t.ID,
			"kind":        t.Kind,
			"state":       t.State,
			"code":        t.Code,
			"paths":       t.Paths,
			"bytes_done":  t.BytesDone,
			"error":       t.Error,
			"created_at":  t.CreatedAt.Format(time.RFC3339),
			"finished_at": finishedOrEmpty(t.FinishedAt),
		})
	}
	okData(c, map[string]any{"tasks": items})
}

func finishedOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// AdminP2pSend 发起发送。
// @router /admin/p2p/send [POST]
func AdminP2pSend(ctx context.Context, c *app.RequestContext) {
	var req p2p.AdminP2pSendReq
	if err := c.BindAndValidate(&req); err != nil {
		badReq(c, "请求体无效（需 paths 数组）")
		return
	}
	task, err := getP2px().Send(ctx, req.Paths)
	audit(ctx, "p2p.send", strings.Join(req.Paths, ","), err)
	if err != nil {
		badReq(c, err.Error())
		return
	}
	snap := task.Snapshot()
	okData(c, map[string]any{"task": taskToModel(&snap)})
}

// AdminP2pRecv 发起接收。
// @router /admin/p2p/recv [POST]
func AdminP2pRecv(ctx context.Context, c *app.RequestContext) {
	var req p2p.AdminP2pRecvReq
	if err := c.BindAndValidate(&req); err != nil {
		badReq(c, "请求体无效（需 code）")
		return
	}
	task, err := getP2px().Recv(ctx, req.Code)
	audit(ctx, "p2p.recv", req.Code, err)
	if err != nil {
		badReq(c, err.Error())
		return
	}
	snap := task.Snapshot()
	okData(c, map[string]any{"task": taskToModel(&snap)})
}

// AdminP2pCancel 取消任务。
// @router /admin/p2p/cancel [POST]
func AdminP2pCancel(ctx context.Context, c *app.RequestContext) {
	var req p2p.AdminP2pCancelReq
	if err := c.BindAndValidate(&req); err != nil {
		badReq(c, "请求体无效（需 task_id）")
		return
	}
	err := getP2px().Cancel(ctx, req.TaskID)
	audit(ctx, "p2p.cancel", req.TaskID, err)
	if err != nil {
		badReq(c, err.Error())
		return
	}
	okData(c, map[string]any{"task_id": req.TaskID})
}

// AdminP2pPublish 收件发布为分享（每文件一个分享；复用本地导入铸链）。
// @router /admin/p2p/publish [POST]
func AdminP2pPublish(ctx context.Context, c *app.RequestContext) {
	var req p2p.AdminP2pPublishReq
	if err := c.BindAndValidate(&req); err != nil || req.TaskID == "" {
		badReq(c, "请求体无效（需 task_id）")
		return
	}
	paths, err := getP2px().PublishPaths(ctx, req.TaskID)
	if err != nil {
		badReq(c, err.Error())
		return
	}
	// 过期样式/密码处理与本地导入同链路（gen/handler/admin/localfiles_ops.go）
	expireStyle := req.GetExpireStyle()
	expireValue := req.GetExpireValue()
	if expireStyle == "" {
		expireStyle = "day"
		if expireValue <= 0 {
			expireValue = 7
		}
	}
	if err := utils.CheckExpireStyleAllowed(expireStyle); err != nil {
		badReq(c, err.Error())
		return
	}
	passwordHash, err := utils.ResolveSharePassword(req.GetRequireAuth(), req.GetPassword())
	if err != nil {
		badReq(c, err.Error())
		return
	}
	codes := make([]string, 0, len(paths))
	var firstErr error
	for _, p := range paths {
		result, ierr := getShare().ImportLocalFile(ctx, shareService.ImportLocalOpts{
			AbsPath:      p,
			ExpireValue:  int(expireValue),
			ExpireStyle:  expireStyle,
			RequireAuth:  req.GetRequireAuth(),
			PasswordHash: passwordHash,
			CustomCode:   req.GetCustomCode(),
			OwnerIP:      middleware.ClientIP(c),
			Internal:     true, // 直传发布：p2px 已做收件区白名单校验，跳过 local_import 门
		})
		if ierr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", filepath.Base(p), ierr)
			}
			continue
		}
		codes = append(codes, result.Code)
	}
	audit(ctx, "p2p.publish", req.TaskID, firstErr)
	if len(codes) == 0 && firstErr != nil {
		badReq(c, firstErr.Error())
		return
	}
	okData(c, map[string]any{"share_codes": codes})
}
