// gen/handler/p2p/p2p_service.go — 设备直传管理面（P2a，2026-10-14 对称节点
// spec §7/§8）。hz 首生成桩后本文件转手写（模型引 contracts/gen/p2p，
// hzmodel 临时目录不落地）。全部端点挂 /admin 组（AdminMiddleware），操作
// 留审计；数据面=supervisor spawn p2pc 子进程，本层只做编排，不代理文件流量。
package p2p

import (
	"context"
	"fmt"
	"github.com/pigeonbox/contracts/errcode"
	"io"
	"os"
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
		ID:         t.ID,
		Kind:       t.Kind,
		State:      t.State,
		Code:       t.Code,
		Paths:      t.Paths,
		BytesDone:  t.BytesDone,
		BytesTotal: t.BytesTotal,
		Error:      t.Error,
		Attempts:   int32(t.Attempts),
		CreatedAt:  t.CreatedAt.Format(time.RFC3339),
	}
	if !t.FinishedAt.IsZero() {
		m.FinishedAt = t.FinishedAt.Format(time.RFC3339)
	}
	return m
}

func badReq(c *app.RequestContext, msg string) {
	c.JSON(consts.StatusBadRequest, map[string]any{"code": errcode.CodeInvalidParam, "message": msg})
}

func okData(c *app.RequestContext, data map[string]any) {
	c.JSON(consts.StatusOK, map[string]any{"code": 0, "message": "success", "data": data})
}

// 审计（与 admin 域同表 admin_operation_logs；wiring 注入写者，nil=跳过）。
var auditWriter func(ctx context.Context, action, target string, success bool)

// SetAuditWriter 注入审计写者（wiring 装配）。
func SetAuditWriter(fn func(ctx context.Context, action, target string, success bool)) {
	auditWriter = fn
}

// audit 操作审计（口令打码——口令即授权,bearer 不落明文日志,只留前 4 位）。
func audit(ctx context.Context, action, target string, err error) {
	if auditWriter != nil {
		auditWriter(ctx, action, target, err == nil)
	}
}

// maskCode 口令打码（审计用）:KJ8Y-WCYZ-E249 → KJ8Y-****-****
func maskCode(code string) string {
	code = strings.TrimSpace(code)
	if len(code) <= 4 {
		return "****"
	}
	return code[:4] + "-****-****"
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
			"bytes_total": t.BytesTotal,
			"error":       t.Error,
			"attempts":    t.Attempts,
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

// AdminP2pTaskLog 任务日志（p2pc 行级环,时间正序;诊断面——失败免登服务器）。
// @router /admin/p2p/tasks/:task_id/log [GET]
func AdminP2pTaskLog(ctx context.Context, c *app.RequestContext) {
	lines, err := getP2px().TaskLog(ctx, c.Param("task_id"))
	if err != nil {
		c.JSON(consts.StatusNotFound, map[string]any{"code": errcode.CodeNotFound, "message": err.Error()})
		return
	}
	okData(c, map[string]any{"lines": lines})
}

// AdminP2pSend 发起发送。
// @router /admin/p2p/send [POST]
func AdminP2pSend(ctx context.Context, c *app.RequestContext) {
	var req p2p.AdminP2pSendReq
	if err := c.BindAndValidate(&req); err != nil {
		badReq(c, "请求体无效（需 paths 数组）")
		return
	}
	opts := []p2px.SendOption{}
	if to := strings.TrimSpace(req.GetInviteTo()); to != "" {
		opts = append(opts, p2px.WithInvite(to, req.GetInviteNote()))
	}
	task, err := getP2px().Send(ctx, req.Paths, opts...)
	audit(ctx, "p2p.send", strings.Join(req.Paths, ","), err)
	if err != nil {
		badReq(c, err.Error())
		return
	}
	snap := task.Snapshot()
	okData(c, map[string]any{"task": taskToModel(&snap)})
}

// uploadSendMaxFiles 浏览器直发单次文件数上限（p2pc send 参数表容量充足，
// 上限只防滥用）。
const uploadSendMaxFiles = 20

// AdminP2pUploadSend 浏览器文件直发（2026-10-15:p2pcweb 体验融入管理台）。
// multipart files[] 流式落上传临时区 → p2px.SendUploaded 编排（上传目录
// 自带发送授权,不查 send_roots——见 upload.go 授权模型）→ 任务终态自清理。
// invite 元数据走 query（与 /admin/p2p/send 同义）。
// 总量闸 = p2p.direct.max_task_bytes 单任务配额,落盘边写边计,超限即断。
// @router /admin/p2p/send/upload [POST]
func AdminP2pUploadSend(ctx context.Context, c *app.RequestContext) {
	svc := getP2px()
	quota, err := svc.UploadQuotaBytes(ctx)
	if err != nil {
		badReq(c, err.Error())
		return
	}
	form, err := c.MultipartForm()
	if err != nil {
		badReq(c, "解析 multipart 失败: "+err.Error())
		return
	}
	files := form.File["files"]
	if len(files) == 0 {
		files = form.File["file"]
	}
	if len(files) == 0 || len(files) > uploadSendMaxFiles {
		badReq(c, fmt.Sprintf("files 需为 1-%d 项", uploadSendMaxFiles))
		return
	}

	dir, err := svc.NewUploadDir()
	if err != nil {
		badReq(c, err.Error())
		return
	}
	paths := make([]string, 0, len(files))
	var total int64
	var overflow bool
	for _, fh := range files {
		name := uploadSafeName(fh.Filename)
		dst := filepath.Join(dir, name)
		src, oerr := fh.Open()
		if oerr != nil {
			overflow = true
			break
		}
		f, cerr := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if cerr != nil {
			_ = src.Close()
			overflow = true
			break
		}
		n, cerr := io.Copy(f, src)
		serr := src.Close()
		ferr := f.Close()
		if cerr != nil || serr != nil || ferr != nil {
			overflow = true
			break
		}
		total += n
		if total > quota {
			overflow = true
			break
		}
		paths = append(paths, dst)
	}
	if overflow {
		_ = os.RemoveAll(dir) // 半截上传即清,不留垃圾
		if total > quota {
			badReq(c, fmt.Sprintf("上传总量超过单任务配额（%d > %d 字节）", total, quota))
			return
		}
		badReq(c, "上传落盘失败")
		return
	}
	if len(paths) == 0 {
		_ = os.RemoveAll(dir)
		badReq(c, "缺少 files 字段")
		return
	}

	opts := []p2px.SendOption{}
	if to := strings.TrimSpace(c.Query("invite_to")); to != "" {
		opts = append(opts, p2px.WithInvite(to, c.Query("invite_note")))
	}
	task, err := svc.SendUploaded(ctx, dir, paths, opts...)
	audit(ctx, "p2p.upload_send", fmt.Sprintf("%d files, %d bytes", len(paths), total), err)
	if err != nil {
		_ = os.RemoveAll(dir) // 编排失败（并发上限/未启用等）不留孤儿
		badReq(c, err.Error())
		return
	}
	snap := task.Snapshot()
	okData(c, map[string]any{"task": taskToModel(&snap)})
}

// uploadSafeName 上传文件名消毒：剥路径分量与控制字符,保留中文/空格,
// 空名兜底,超长截断（对端 manifest 呈现原始名,这里只保落盘安全）。
func uploadSafeName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		name = "file"
	}
	if len(name) > 180 {
		name = name[:180]
	}
	return name
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
	audit(ctx, "p2p.recv", maskCode(req.Code), err)
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
	if firstErr == nil {
		getP2px().MarkPublished(req.TaskID) // 全成功才置幂等闸（部分失败允许重试补齐）
	}
	okData(c, map[string]any{"share_codes": codes})
}

// AdminP2pInvites 待收邀请列表（P3）。
// @router /admin/p2p/invites [GET]
func AdminP2pInvites(ctx context.Context, c *app.RequestContext) {
	invites := getP2px().ListInvites()
	items := make([]map[string]any, 0, len(invites))
	for _, iv := range invites {
		items = append(items, map[string]any{
			"id":          iv.ID,
			"code":        iv.Code,
			"from_name":   iv.FromName,
			"note":        iv.Note,
			"received_at": iv.ReceivedAt.Format(time.RFC3339),
			"expires_in":  iv.ExpiresIn,
		})
	}
	okData(c, map[string]any{"invites": items})
}

// AdminP2pInviteAccept 认领邀请并发起接收（P3）。
// @router /admin/p2p/invites/accept [POST]
func AdminP2pInviteAccept(ctx context.Context, c *app.RequestContext) {
	var req p2p.AdminP2pInviteAcceptReq
	if err := c.BindAndValidate(&req); err != nil || req.GetInviteID() == "" {
		badReq(c, "请求体无效（需 invite_id）")
		return
	}
	inv, err := getP2px().TakeInvite(req.GetInviteID())
	if err != nil {
		badReq(c, err.Error())
		return
	}
	task, err := getP2px().Recv(ctx, inv.Code)
	audit(ctx, "p2p.invite.accept", inv.ID, err)
	if err != nil {
		badReq(c, err.Error())
		return
	}
	snap := task.Snapshot()
	okData(c, map[string]any{"task": taskToModel(&snap)})
}

// AdminP2pInvitePush 把口令推送到目标实例（P3 免口令交接）。
// @router /admin/p2p/invite-push [POST]
func AdminP2pInvitePush(ctx context.Context, c *app.RequestContext) {
	var req p2p.AdminP2pInvitePushReq
	if err := c.BindAndValidate(&req); err != nil || req.GetCode() == "" || req.GetTargetURL() == "" {
		badReq(c, "请求体无效（需 code 与 target_url）")
		return
	}
	err := p2px.PushInvite(ctx, req.GetTargetURL(), req.GetCode(), req.GetFromName(), req.GetNote())
	audit(ctx, "p2p.invite.push", req.GetTargetURL(), err)
	if err != nil {
		badReq(c, err.Error())
		return
	}
	okData(c, map[string]any{"target_url": req.GetTargetURL()})
}

// AdminP2pDirectory 在线名录透传（2026-10-14 邀请信箱配套:发送卡对端选择器
// 消费;p2pd 名录零敏感,本层不改写）。
// @router /admin/p2p/directory [GET]
func AdminP2pDirectory(ctx context.Context, c *app.RequestContext) {
	peers, err := getP2px().DirectoryPeers(ctx)
	if err != nil {
		c.JSON(consts.StatusInternalServerError, map[string]any{"code": errcode.CodeInternal, "message": err.Error()})
		return
	}
	out := make([]*p2p.P2pDirectoryPeer, 0, len(peers))
	for _, p := range peers {
		out = append(out, &p2p.P2pDirectoryPeer{
			NodeID:      p.NodeID,
			Name:        p.Name,
			URL:         p.URL,
			BoxPub:      p.BoxPub,
			OnlineSince: p.OnlineSince,
		})
	}
	okData(c, map[string]any{"peers": out})
}
