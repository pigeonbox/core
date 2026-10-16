package bootstrap

// 职责：composition root 适配器——pkg 层持久化 dao 桥与域间窄接口适配器
// （mcp/request/user 消费侧），跨域依赖在此收口。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	adminApp "github.com/pigeonbox/core/app/admin"
	configApp "github.com/pigeonbox/core/app/config"
	federationApp "github.com/pigeonbox/core/app/federation"
	mcpApp "github.com/pigeonbox/core/app/mcp"
	p2pxApp "github.com/pigeonbox/core/app/p2px"
	presignApp "github.com/pigeonbox/core/app/presign"
	previewApp "github.com/pigeonbox/core/app/preview"
	requestApp "github.com/pigeonbox/core/app/request"
	shareService "github.com/pigeonbox/core/app/share"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/pkg/transfer"
	"github.com/pigeonbox/core/repo/db"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
)

// ===== pkg 层持久化桥 =====
// pkg/middleware、pkg/transfer 不直接依赖 repo 层（底层包方向纯净），
// 持久化能力由 composition root 在此以 dao 实现注入。

// daoAPIKeyStore pkg/middleware.APIKeyStore 的 dao 桥。
type daoAPIKeyStore struct{}

func (daoAPIKeyStore) FindActiveByHash(ctx context.Context, hash string) (*middleware.APIKeyPrincipal, error) {
	key, err := dao.NewUserAPIKeyRepository(db.GetDB()).GetActiveByHash(ctx, hash) // 装配层显式句柄
	if err != nil {
		return nil, err
	}
	user, err := dao.NewUserRepository(db.GetDB()).GetByID(ctx, key.UserID)
	if err != nil || user.Status != "active" {
		// 封禁/停用用户的 Key 视为无效 Key（调用方计入防爆破，防枚举）
		return nil, fmt.Errorf("api key invalid")
	}
	return &middleware.APIKeyPrincipal{KeyID: key.ID, UserID: user.ID, Username: user.Username, Role: user.Role}, nil
}

func (daoAPIKeyStore) TouchLastUsed(ctx context.Context, keyID uint, ip string) error {
	return dao.NewUserAPIKeyRepository(db.GetDB()).TouchLastUsed(ctx, keyID, ip) // 装配层显式句柄
}

// daoTransferSink pkg/transfer.Sink 的 dao 桥。
type daoTransferSink struct{}

func (daoTransferSink) Create(ctx context.Context, e transfer.Entry) error {
	return dao.NewTransferLogRepository(db.GetDB()).Create(ctx, &model.TransferLog{ // 装配层显式句柄
		Operation: e.Operation, FileCodeID: e.FileCodeID, FileCode: e.Code,
		FileName: e.FileName, FileSize: e.FileSize, UserID: e.UserID,
		APIKeyID: e.APIKeyID, Username: e.Username, IP: e.IP, DurationMs: e.DurationMs,
	})
}

// ===== 域间装配适配器 =====
// 消费侧窄接口（mcp.AdminAPI/ShareAPI、request.ShareGateway、user.DefaultsProvider）
// 的域适配器：跨域依赖收口到 composition root，各业务域互相零 import。

// mcpAdminAdapter mcp.AdminAPI 的 admin 域适配器（全站唯一实例）。
type mcpAdminAdapter struct{ svc *adminApp.Service }

func (a mcpAdminAdapter) DeleteShareByID(ctx context.Context, id uint) error {
	return a.svc.DeleteFile(ctx, id)
}

func (a mcpAdminAdapter) SystemStats(ctx context.Context) (*mcpApp.SystemStats, error) {
	st, err := a.svc.GetStats(ctx)
	if err != nil {
		return nil, err
	}
	return &mcpApp.SystemStats{
		TotalFiles: st.TotalFiles, TotalUsers: st.TotalUsers, TotalSize: st.TotalSize,
		TodayUploads: st.TodayUploads, ExpiredFiles: st.ExpiredFiles,
	}, nil
}

func (a mcpAdminAdapter) StorageStatus(ctx context.Context) (*mcpApp.StorageStatusInfo, error) {
	st, err := a.svc.GetStorageStatus(ctx)
	if err != nil {
		return nil, err
	}
	return &mcpApp.StorageStatusInfo{
		StorageType: st.StorageType, TotalSpace: st.TotalSpace, UsedSpace: st.UsedSpace,
		UsagePercent: st.UsagePercent, FileCount: st.FileCount,
	}, nil
}

func (a mcpAdminAdapter) Users(ctx context.Context, page, pageSize int) ([]mcpApp.UserRow, int64, error) {
	users, total, err := a.svc.GetUsers(ctx, page, pageSize)
	if err != nil {
		return nil, 0, err
	}
	rows := make([]mcpApp.UserRow, len(users))
	for i, u := range users {
		rows[i] = mcpApp.UserRow{ID: u.ID, Username: u.Username, Email: u.Email, Status: u.Status}
	}
	return rows, total, nil
}

func (a mcpAdminAdapter) CleanExpired(ctx context.Context) (int64, int64, error) {
	return a.svc.CleanExpiredFiles(ctx)
}

// mcpShareAdapter mcp.ShareAPI 的 share 域适配器。
type mcpShareAdapter struct{ svc *shareService.Service }

func (a mcpShareAdapter) ShareBytes(ctx context.Context, opts mcpApp.FileShareOpts) (mcpApp.ShareCreated, error) {
	resp, err := a.svc.ShareBytes(ctx, shareService.ShareBytesOpts{
		FileName: opts.FileName, Content: opts.Content,
		ExpireValue: opts.ExpireValue, ExpireStyle: opts.ExpireStyle,
		PasswordHash: opts.PasswordHash, CustomCode: opts.CustomCode,
		UserID: opts.UserID, OwnerIP: opts.OwnerIP,
	})
	if err != nil {
		return mcpApp.ShareCreated{}, err
	}
	return mcpApp.ShareCreated{Code: resp.Code, PickupCode: resp.PickupCode, FullShareURL: resp.FullShareURL}, nil
}

func (a mcpShareAdapter) CreateTextShare(ctx context.Context, text string, expireValue int, expireStyle string,
	requireAuth bool, passwordHash string, userID *uint, ownerIP, customCode string) (mcpApp.ShareCreated, error) {
	resp, err := a.svc.ShareTextWithAuth(ctx, text, expireValue, expireStyle, requireAuth, passwordHash, userID, ownerIP, false, customCode)
	if err != nil {
		return mcpApp.ShareCreated{}, err
	}
	return mcpApp.ShareCreated{Code: resp.Code, PickupCode: resp.PickupCode, FullShareURL: resp.FullShareURL}, nil
}

func (a mcpShareAdapter) ShareFiles(ctx context.Context, code string) ([]mcpApp.ShareFileInfo, error) {
	items, err := a.svc.ListShareFiles(ctx, code)
	if err != nil {
		return nil, err
	}
	out := make([]mcpApp.ShareFileInfo, len(items))
	for i, it := range items {
		out[i] = mcpApp.ShareFileInfo{Name: it.Name, Size: it.Size}
	}
	return out, nil
}

// mcpRequestAdapter mcp.RequestAPI 的 request 域适配器（create_request_code）。
type mcpRequestAdapter struct{ svc *requestApp.Service }

func (a mcpRequestAdapter) Create(ctx context.Context, userID uint, req mcpApp.RequestCreate) (string, time.Time, error) {
	fr, err := a.svc.Create(ctx, userID, requestApp.CreateReq{
		Title: req.Title, MaxFiles: req.MaxFiles, MaxBytes: req.MaxBytes,
		ExpireValue: req.ExpireValue, ExpireStyle: req.ExpireStyle,
	})
	if err != nil {
		return "", time.Time{}, err
	}
	exp := time.Time{}
	if fr.ExpiredAt != nil {
		exp = *fr.ExpiredAt
	}
	return fr.Token, exp, nil
}

// mcpPreviewAdapter mcp.PreviewAPI 的 preview 域适配器（preview_share）。
type mcpPreviewAdapter struct{ svc *previewApp.Service }

func (a mcpPreviewAdapter) Preview(ctx context.Context, req mcpApp.PreviewReq) (*mcpApp.PreviewResult, error) {
	fp, err := a.svc.GetOrCreate(ctx, previewApp.GetReq{
		Code: req.Code, UserID: req.UserID, ClientIP: req.ClientIP, Password: req.Password,
	})
	if err != nil {
		return nil, err
	}
	return &mcpApp.PreviewResult{
		PreviewType: fp.PreviewType, MimeType: fp.MimeType, TextContent: fp.TextContent,
		FileSize: fp.FileSize, Width: fp.Width, Height: fp.Height,
		PageCount: fp.PageCount, Duration: fp.Duration,
	}, nil
}

// mcpFederationAdapter mcp.FederationAPI 的 federation 域适配器。
// svc 为 nil（未启用/初始化失败）时上报"未启用"事实而非报错。
type mcpFederationAdapter struct{ svc *federationApp.Service }

func (a mcpFederationAdapter) Status() mcpApp.FederationStatus {
	if a.svc == nil {
		return mcpApp.FederationStatus{}
	}
	return mcpApp.FederationStatus{
		Enabled: true, NodeID: a.svc.NodeID(), Healthy: a.svc.Healthy(),
		Registries: a.svc.RegistryURLs(),
	}
}

func (a mcpFederationAdapter) Resolve(code string) (*mcpApp.FederationResolve, error) {
	if a.svc == nil {
		return nil, errors.New("联邦未启用")
	}
	info, err := a.svc.Resolve(code)
	if err != nil || info == nil {
		return nil, err
	}
	return &mcpApp.FederationResolve{
		NodeID: info.NodeID, URL: info.URL, Name: info.Name,
		ExpiresAt: info.ExpiresAt, SizeHint: info.SizeHint,
	}, nil
}

// mcpP2pAdapter mcp.P2pAPI 的 p2px 域适配器（P2b 薄工具，spec §9）。
// svc 为 nil（未装配）时上报"未装配"事实而非 panic。
type mcpP2pAdapter struct{ svc *p2pxApp.Service }

func (a mcpP2pAdapter) P2pStatus(ctx context.Context) (mcpApp.P2pStatus, error) {
	if a.svc == nil {
		return mcpApp.P2pStatus{}, errors.New("直传 supervisor 未装配")
	}
	st, err := a.svc.Status(ctx)
	if err != nil {
		return mcpApp.P2pStatus{}, err
	}
	return mcpApp.P2pStatus{
		Available: st.Available, Enabled: st.Enabled, DeployAllowed: st.DeployAllowed,
		BinaryPath: st.BinaryPath, RegistryURL: st.RegistryURL, RecvDir: st.RecvDir,
		InboxUsed: st.InboxUsed, InboxQuota: st.InboxQuota,
	}, nil
}

func (a mcpP2pAdapter) P2pTasks(ctx context.Context) ([]mcpApp.P2pTaskInfo, error) {
	if a.svc == nil {
		return nil, errors.New("直传 supervisor 未装配")
	}
	tasks := a.svc.List(ctx)
	out := make([]mcpApp.P2pTaskInfo, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, mcpApp.P2pTaskInfo{
			ID: t.ID, Kind: t.Kind, State: t.State, Code: t.Code,
			BytesDone: t.BytesDone, Error: t.Error,
			CreatedAt:  t.CreatedAt.Format(time.RFC3339),
			FinishedAt: finishedTimeStr(t.FinishedAt),
		})
	}
	return out, nil
}

// P2pSend 发起发送（口令由 p2pc stdout 异步解析——有界短轮询等口令，
// 典型毫秒级；超时则返回空码，MCP 侧提示走 p2p_tasks）。
func (a mcpP2pAdapter) P2pSend(ctx context.Context, paths []string, inviteTo, inviteNote string) (mcpApp.P2pTaskInfo, error) {
	if a.svc == nil {
		return mcpApp.P2pTaskInfo{}, errors.New("直传 supervisor 未装配")
	}
	opts := []p2pxApp.SendOption{}
	if strings.TrimSpace(inviteTo) != "" {
		opts = append(opts, p2pxApp.WithInvite(inviteTo, inviteNote))
	}
	task, err := a.svc.Send(ctx, paths, opts...)
	if err != nil {
		return mcpApp.P2pTaskInfo{}, err
	}
	for i := 0; i < 5; i++ {
		snap := task.Snapshot()
		if snap.Code != "" {
			return mcpApp.P2pTaskInfo{
				ID: snap.ID, Kind: snap.Kind, State: snap.State, Code: snap.Code,
				CreatedAt: snap.CreatedAt.Format(time.RFC3339),
			}, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	snap := task.Snapshot()
	return mcpApp.P2pTaskInfo{
		ID: snap.ID, Kind: snap.Kind, State: snap.State, Code: snap.Code,
		CreatedAt: snap.CreatedAt.Format(time.RFC3339),
	}, nil
}

func (a mcpP2pAdapter) P2pRecv(ctx context.Context, code string) (mcpApp.P2pTaskInfo, error) {
	if a.svc == nil {
		return mcpApp.P2pTaskInfo{}, errors.New("直传 supervisor 未装配")
	}
	task, err := a.svc.Recv(ctx, code)
	if err != nil {
		return mcpApp.P2pTaskInfo{}, err
	}
	snap := task.Snapshot()
	return mcpApp.P2pTaskInfo{
		ID: snap.ID, Kind: snap.Kind, State: snap.State, Code: snap.Code,
		CreatedAt: snap.CreatedAt.Format(time.RFC3339),
	}, nil
}

// P2pPeers 在线名录透传（p2px DirectoryPeers；零敏感）。
func (a mcpP2pAdapter) P2pPeers(ctx context.Context) ([]mcpApp.P2pPeerInfo, error) {
	if a.svc == nil {
		return nil, errors.New("直传 supervisor 未装配")
	}
	peers, err := a.svc.DirectoryPeers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]mcpApp.P2pPeerInfo, 0, len(peers))
	for _, p := range peers {
		out = append(out, mcpApp.P2pPeerInfo{
			NodeID: p.NodeID, Name: p.Name, URL: p.URL,
			BoxPub: p.BoxPub, OnlineSince: p.OnlineSince,
		})
	}
	return out, nil
}

// P2pInvites 待收邀请箱快照（P3 收件箱）。
func (a mcpP2pAdapter) P2pInvites(ctx context.Context) ([]mcpApp.P2pInviteInfo, error) {
	if a.svc == nil {
		return nil, errors.New("直传 supervisor 未装配")
	}
	invites := a.svc.ListInvites()
	out := make([]mcpApp.P2pInviteInfo, 0, len(invites))
	for _, it := range invites {
		out = append(out, mcpApp.P2pInviteInfo{
			ID: it.ID, Code: it.Code, FromName: it.FromName,
			Note: it.Note, ExpiresIn: it.ExpiresIn,
		})
	}
	return out, nil
}

// P2pAccept 认领邀请并立即发起接收（与 Web 待收面板 accept 同链路）。
func (a mcpP2pAdapter) P2pAccept(ctx context.Context, inviteID string) (mcpApp.P2pTaskInfo, error) {
	if a.svc == nil {
		return mcpApp.P2pTaskInfo{}, errors.New("直传 supervisor 未装配")
	}
	inv, err := a.svc.TakeInvite(inviteID)
	if err != nil {
		return mcpApp.P2pTaskInfo{}, err
	}
	task, err := a.svc.Recv(ctx, inv.Code)
	if err != nil {
		// 认领已出箱但接收发起失败:邀请回箱(重试不丢码)
		if _, re := a.svc.AddInvite(inv.Code, inv.FromName, inv.Note); re != nil {
			return mcpApp.P2pTaskInfo{}, fmt.Errorf("接收发起失败且邀请回箱失败(%v): %w", re, err)
		}
		return mcpApp.P2pTaskInfo{}, err
	}
	snap := task.Snapshot()
	return mcpApp.P2pTaskInfo{
		ID: snap.ID, Kind: snap.Kind, State: snap.State, Code: snap.Code,
		CreatedAt: snap.CreatedAt.Format(time.RFC3339),
	}, nil
}

// finishedTimeStr 终态时间字符串（零值 → 空串）。
func finishedTimeStr(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// requestShareGateway request.ShareGateway 的 share 域适配器（访客投递 → 归属分享）。
type requestShareGateway struct{ svc *shareService.Service }

func (g requestShareGateway) CreateFromFileEntries(ctx context.Context, req *requestApp.ShareCreateRequest) (*requestApp.ShareResult, error) {
	ownerID := req.OwnerID
	resp, err := g.svc.CreateMultiFileShare(ctx, &shareService.MultiShareReq{
		Entries:      req.Entries,
		ExpiredAt:    req.ExpiredAt,
		ExpiredCount: req.ExpiredCount,
		UserID:       &ownerID,
		UploadType:   "authenticated",
		OwnerIP:      req.OwnerIP,
		Channel:      req.Channel,
	})
	if err != nil {
		return nil, err
	}
	return &requestApp.ShareResult{ID: resp.ID, Code: resp.Code, PickupCode: resp.PickupCode, Size: resp.Size}, nil
}

// presignShareAdapter presign.ShareServiceInterface 的 share 域适配器
// （此前 presign 直接 import share.ShareFileReq/ShareResp 构成唯一约定
// app 跨域边，2026-10-10 消解——消费侧窄类型 + 装配点转换，与
// requestShareGateway/mcpShareAdapter 同款模式）。
type presignShareAdapter struct{ svc *shareService.Service }

func (a presignShareAdapter) CreateShare(ctx context.Context, req *presignApp.ShareCreateReq) (*presignApp.ShareCreateResp, error) {
	resp, err := a.svc.CreateShare(ctx, &shareService.ShareFileReq{
		Channel:      req.Channel,
		FilePath:     req.FilePath,
		Size:         req.Size,
		Text:         req.Text,
		ExpiredAt:    req.ExpiredAt,
		ExpiredCount: req.ExpiredCount,
		RequireAuth:  req.RequireAuth,
		PasswordHash: req.PasswordHash,
		UserID:       req.UserID,
		UploadType:   req.UploadType,
		OwnerIP:      req.OwnerIP,
		FileHash:     req.FileHash,
		UploadID:     req.UploadID,
		Note:         req.Note,
	})
	if err != nil {
		return nil, err
	}
	return &presignApp.ShareCreateResp{
		Code:         resp.Code,
		ShareURL:     resp.ShareURL,
		FullShareURL: resp.FullShareURL,
		PickupCode:   resp.PickupCode,
	}, nil
}

// adminDefaultsAdapter user.DefaultsProvider 的 admin 域适配器
// （管理后台持久化配置优先，无记录回退 yaml——EffectiveUserSettings 语义）。
type adminDefaultsAdapter struct{}

func (adminDefaultsAdapter) DefaultStorageQuota(ctx context.Context) int64 {
	return configApp.EffectiveUserSettings(ctx).UserStorageQuota
}

func (adminDefaultsAdapter) DefaultUploadSize(ctx context.Context) int64 {
	return configApp.EffectiveUserSettings(ctx).UserUploadSize
}

func (adminDefaultsAdapter) AllowUserRegistration(ctx context.Context) bool {
	return configApp.EffectiveUserSettings(ctx).AllowUserRegistration
}

// userEmailLookup notify.SMTPMailer.UserEmailLookup 的 dao 桥（SendToUser 查邮箱）。
type userEmailLookup struct{}

func (userEmailLookup) EmailByID(ctx context.Context, id uint) (string, error) {
	u, err := dao.NewUserRepository(db.GetDB()).GetByID(ctx, id) // 装配层显式句柄
	if err != nil {
		return "", err
	}
	return u.Email, nil
}
