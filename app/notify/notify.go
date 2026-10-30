// Package notify 实现系统通知（管理员公告 + per-user 取件通知）service。
//
// 分层：通过 dao.NotifyRepository 访问数据（与其他 service 一致），
// 不再直接持有 *gorm.DB。
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/oekit/concurrency/async"
	"github.com/oekit/pkg/contextx"
	"github.com/oekit/pkg/httpjson"
	"github.com/pigeonbox/core/internal/paging"
	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/pkg/security"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
	"sync/atomic"
)

// webhookHTTPClient webhook 推送专用客户端（5s 超时；推送失败静默记日志不重试）。
// 重定向防护（2026-10-05 审计 P3）：webhook URL 仅管理员可配，但保存期校验
// 会被 302 绕过——目标服务器一跳转即可指向私网。CheckRedirect 逐跳复用
// security.ValidateEndpointURL（respect security.ssrf.allow_private_networks），
// 私网目标且未放行时中止。
var webhookHTTPClient = &http.Client{
	Timeout: 5 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("stopped after 5 redirects")
		}
		if err := security.ValidateEndpointURL(req.URL.String()); err != nil {
			return fmt.Errorf("webhook 重定向目标被拒绝: %w", err)
		}
		return nil
	},
}

// 错误
var (
	ErrNotifyNotFound = errors.New("notify not found")
	ErrInvalidParam   = errors.New("invalid parameter")
)

// Service 通知 service
type Service struct {
	notifyRepo *dao.NotifyRepository

	// webhookURL 外部 Webhook 推送地址（空 = 禁用）。
	//
	// ⚠️ **原子持有**（2026-10-31 修）：此前是**裸 string 字段**，而
	// SetWebhookURL 由 bootstrap 的 OnNotifyChanged 钩子在**运行期**调用
	// （管理员在管理台保存通知配置 ⇒ 整站热重建），读点则分布在事件推送
	// 路径上（请求并发）⇒ **data race**。
	//
	// ⚠️ 且 string 是 **16 字节**（数据指针 + 长度）⇒ 非原子写 ⇒ 并发读可能
	// 拿到**新指针配旧长度**（撕裂）⇒ 越界读/崩溃。
	//
	// ⇒ 改用 atomic.Pointer[string]：写侧整体换指针，读侧 Load() 一次拿到
	// 一致快照（要么全旧、要么全新）。
	webhookURL atomic.Pointer[string]

	// mailer 邮件通知渠道（nil = 禁用；P2 SMTP）。
	//
	// ⚠️ 同上：运行期热重建写、请求路径读，无锁 ⇒ data race。
	mailer atomic.Pointer[SMTPMailer]
}

// webhookTarget 当前生效的 Webhook 地址（空串 = 禁用）。
func (s *Service) webhookTarget() string {
	if p := s.webhookURL.Load(); p != nil {
		return *p
	}
	return ""
}

// NewService 创建 service（repo 构造注入，去全局化终态）
func NewService(gormDB *gorm.DB) *Service {
	return &Service{notifyRepo: dao.NewNotifyRepository(gormDB)}
}

// ListItem 列表项
type ListItem struct {
	ID        uint       `json:"id"`
	Title     string     `json:"title"`
	Content   string     `json:"content"`
	Type      string     `json:"type"`
	Level     string     `json:"level"`
	Status    int        `json:"status"`
	StartAt   *time.Time `json:"start_at,omitempty"`
	EndAt     *time.Time `json:"end_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// ListData 列表数据
type ListData struct {
	Items    []ListItem `json:"items"`
	Total    int64      `json:"total"`
	Page     int        `json:"page"`
	PageSize int        `json:"page_size"`
}

// ListReq 列表请求
type ListReq struct {
	Page     int
	PageSize int
	Type     string
	Level    string
	Status   *int
}

// List 列表
func (s *Service) List(ctx context.Context, req ListReq) (*ListData, error) {
	req.Page, req.PageSize = paging.NotifyPolicy.Normalize(req.Page, req.PageSize)
	tx := s.notifyRepo.Query(ctx)
	if req.Type != "" {
		tx = tx.Where("type = ?", req.Type)
	}
	if req.Level != "" {
		tx = tx.Where("level = ?", req.Level)
	}
	if req.Status != nil {
		tx = tx.Where("status = ?", *req.Status)
	}
	var total int64
	if err := tx.Count(&total).Error; err != nil {
		return nil, err
	}
	var items []model.Notify
	if err := tx.Order("created_at DESC").Offset((req.Page - 1) * req.PageSize).Limit(req.PageSize).Find(&items).Error; err != nil {
		return nil, err
	}
	result := &ListData{
		Items:    make([]ListItem, 0, len(items)),
		Total:    total,
		Page:     req.Page,
		PageSize: req.PageSize,
	}
	for _, n := range items {
		result.Items = append(result.Items, toListItem(&n))
	}
	return result, nil
}

// Get 单条
func (s *Service) Get(ctx context.Context, id uint) (*ListItem, error) {
	n, err := s.notifyRepo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotifyNotFound
		}
		return nil, err
	}
	item := toListItem(n)
	return &item, nil
}

// Active 当前活跃通知（公开 API，供全站横幅）。
// 仅广播（target_user_id 空/0）：定向站内信绝不能从无认证端点泄露，
// 否则取件码/投递详情会广播给匿名访客；定向通知走 /api/v1/notifies/mine。
func (s *Service) Active(ctx context.Context, typ string) ([]ListItem, error) {
	tx := s.notifyRepo.Query(ctx).
		Where("status = ?", 1).
		Where("target_user_id IS NULL OR target_user_id = ?", 0).
		Where("start_at IS NULL OR start_at <= ?", time.Now()).
		Where("end_at IS NULL OR end_at >= ?", time.Now())
	if typ != "" {
		tx = tx.Where("type = ?", typ)
	}
	var items []model.Notify
	if err := tx.Order("created_at DESC").Limit(50).Find(&items).Error; err != nil {
		return nil, err
	}
	result := make([]ListItem, 0, len(items))
	for i := range items {
		result = append(result, toListItem(&items[i]))
	}
	return result, nil
}

// CreateReq 创建请求
type CreateReq struct {
	Title    string
	Content  string
	Type     string
	Level    string
	Status   int
	StartAt  *time.Time
	EndAt    *time.Time
	AuthorID uint
}

// Create 创建
func (s *Service) Create(ctx context.Context, req CreateReq) (*ListItem, error) {
	if req.Title == "" || req.Content == "" {
		return nil, ErrInvalidParam
	}
	if req.Type == "" {
		req.Type = "system"
	}
	if req.Level == "" {
		req.Level = "info"
	}
	if req.Status == 0 {
		req.Status = 1
	}
	n := model.Notify{
		Title:    req.Title,
		Content:  req.Content,
		Type:     req.Type,
		Level:    req.Level,
		Status:   req.Status,
		StartAt:  req.StartAt,
		EndAt:    req.EndAt,
		AuthorID: req.AuthorID,
	}
	if err := s.notifyRepo.Create(ctx, &n); err != nil {
		return nil, err
	}
	item := toListItem(&n)
	return &item, nil
}

// UpdateReq 更新请求
type UpdateReq struct {
	ID      uint
	Title   *string
	Content *string
	Type    *string
	Level   *string
	Status  *int
	StartAt *time.Time
	EndAt   *time.Time
}

// Update 更新
func (s *Service) Update(ctx context.Context, req UpdateReq) error {
	updates := map[string]interface{}{}
	if req.Title != nil {
		updates["title"] = *req.Title
	}
	if req.Content != nil {
		updates["content"] = *req.Content
	}
	if req.Type != nil {
		updates["type"] = *req.Type
	}
	if req.Level != nil {
		updates["level"] = *req.Level
	}
	if req.Status != nil {
		updates["status"] = *req.Status
	}
	if req.StartAt != nil {
		updates["start_at"] = *req.StartAt
	}
	if req.EndAt != nil {
		updates["end_at"] = *req.EndAt
	}
	if len(updates) == 0 {
		return nil
	}
	affected, err := s.notifyRepo.UpdateByID(ctx, req.ID, updates)
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotifyNotFound
	}
	return nil
}

// Delete 删除
func (s *Service) Delete(ctx context.Context, id uint) error {
	affected, err := s.notifyRepo.DeleteByID(ctx, id)
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotifyNotFound
	}
	return nil
}

// ============ 辅助 ============

func toListItem(n *model.Notify) ListItem {
	return ListItem{
		ID:        n.ID,
		Title:     n.Title,
		Content:   n.Content,
		Type:      n.Type,
		Level:     n.Level,
		Status:    n.Status,
		StartAt:   n.StartAt,
		EndAt:     n.EndAt,
		CreatedAt: n.CreatedAt,
		UpdatedAt: n.UpdatedAt,
	}
}

// ParseID 解析 path 中的 id
func ParseID(s string) (uint, error) {
	id, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, ErrInvalidParam
	}
	return uint(id), nil
}

// ============ Per-user 通知（A2 取件通知） ============

// UserNotifyItem 用户通知项
type UserNotifyItem struct {
	ID        uint       `json:"id"`
	Title     string     `json:"title"`
	Content   string     `json:"content"`
	Type      string     `json:"type"`
	Level     string     `json:"level"`
	ReadAt    *time.Time `json:"read_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	IsRead    bool       `json:"is_read"`
}

// UserNotifyListData 用户通知列表
type UserNotifyListData struct {
	Items      []UserNotifyItem `json:"items"`
	Total      int64            `json:"total"`
	Unread     int64            `json:"unread"`
	Page       int              `json:"page"`
	PageSize   int              `json:"page_size"`
	TotalPages int64            `json:"total_pages"`
}

// ListForUser 列出某用户的通知（含广播 + 定向给该用户）
func (s *Service) ListForUser(ctx context.Context, userID uint, page, pageSize int) (*UserNotifyListData, error) {
	page, pageSize = paging.NotifyPolicy.Normalize(page, pageSize)
	q := s.notifyRepo.Query(ctx).
		Where("status = 1").
		Where("target_user_id IS NULL OR target_user_id = ?", userID)

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, err
	}
	unread, err := s.notifyRepo.CountWhere(ctx,
		"status = 1 AND (target_user_id IS NULL OR target_user_id = ?) AND read_at IS NULL",
		[]interface{}{userID})
	if err != nil {
		return nil, err
	}

	var rows []model.Notify
	if err := q.Order("created_at DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&rows).Error; err != nil {
		return nil, err
	}

	items := make([]UserNotifyItem, 0, len(rows))
	for i := range rows {
		n := &rows[i]
		items = append(items, UserNotifyItem{
			ID:        n.ID,
			Title:     n.Title,
			Content:   n.Content,
			Type:      n.Type,
			Level:     n.Level,
			ReadAt:    n.ReadAt,
			CreatedAt: n.CreatedAt,
			IsRead:    n.ReadAt != nil,
		})
	}

	totalPages := (total + int64(pageSize) - 1) / int64(pageSize)
	if totalPages < 1 {
		totalPages = 1
	}
	return &UserNotifyListData{
		Items: items, Total: total, Unread: unread,
		Page: page, PageSize: pageSize, TotalPages: totalPages,
	}, nil
}

// UnreadCountForUser 某用户未读数
func (s *Service) UnreadCountForUser(ctx context.Context, userID uint) (int64, error) {
	return s.notifyRepo.CountWhere(ctx,
		"status = 1 AND (target_user_id IS NULL OR target_user_id = ?) AND read_at IS NULL",
		[]interface{}{userID})
}

// MarkAllReadForUser 标记某用户所有通知为已读（只更新 read_at IS NULL 的）
func (s *Service) MarkAllReadForUser(ctx context.Context, userID uint) (int64, error) {
	return s.notifyRepo.UpdateWhere(ctx,
		"status = 1 AND (target_user_id IS NULL OR target_user_id = ?) AND read_at IS NULL",
		[]interface{}{userID}, "read_at", time.Now())
}

// CreateForUser 创建一条定向通知（owner 取件通知用）
func (s *Service) CreateForUser(ctx context.Context, userID uint, title, content, notifyType, level string) (*model.Notify, error) {
	uid := userID
	n := &model.Notify{
		Title:        title,
		Content:      content,
		Type:         notifyType,
		Level:        level,
		Status:       1,
		AuthorID:     0,
		TargetUserID: &uid,
	}
	if err := s.notifyRepo.Create(ctx, n); err != nil {
		return nil, err
	}
	return n, nil
}

// CreateForUserSimple 简化版（返回 error，匹配 share.NotifyServiceInterface）
func (s *Service) CreateForUserSimple(ctx context.Context, userID uint, title, content, notifyType, level string) error {
	_, err := s.CreateForUser(ctx, userID, title, content, notifyType, level)
	// 外部 Webhook 推送（fire-and-forget；对标上游缺口，双方都缺的外部通知渠道）
	if err == nil {
		s.dispatchWebhook(ctx, userID, title, content, notifyType, level)
		// 邮件补发（fire-and-forget；未配置 SMTP/用户无邮箱时内部短路）
		if m := s.mailer.Load(); m != nil && m.Enabled() {
			// GoSafeLabeled:SMTP 客户端 panic 不打死进程，只记日志（邮件可下次补发）
			// ⚠️ 闭包捕获已 Load 的 m（不是再读 s.mailer）—— 热重建换了 mailer
			// 时，本次发送用**同一个**实例（不会出现"判空时非 nil、发信时已换掉"）。
			async.GoSafeLabeled("notify-mail", func() { m.SendToUser(userID, title, content) })
		}
	}
	return err
}

// SetMailer 注入邮件发送器（bootstrap 从 notify.smtp 段构建；nil = 禁用）
//
// 运行期热重建会调用 ⇒ 必须原子换指针（见 Service.mailer 的说明）。
func (s *Service) SetMailer(m *SMTPMailer) { s.mailer.Store(m) }

// ==================== Webhook 外部通知渠道 ====================

// SetWebhookURL 注入 Webhook 推送地址（bootstrap 从 notify.webhook.url / PB_WEBHOOK_URL 注入；
// 空串 = 禁用）。推送失败只记日志，绝不影响站内信主流程。
func (s *Service) SetWebhookURL(url string) {
	trimmed := strings.TrimSpace(url)
	s.webhookURL.Store(&trimmed)
}

// webhookPayload Webhook POST 的 JSON 载荷
type webhookPayload struct {
	Event      string `json:"event"`
	Title      string `json:"title"`
	Content    string `json:"content"`
	NotifyType string `json:"notify_type"`
	Level      string `json:"level"`
	UserID     uint   `json:"user_id"`
	Timestamp  int64  `json:"timestamp"`
}

// sendWebhook 统一推送出口：发送前对目标 URL 复跑 SSRF 校验（2026-10-08 加固）。
// 此前只有保存期校验（env/file 注入的 PB_WEBHOOK_URL 完全绕过）与 302 逐跳
// 校验——首发请求本身从未被校验，DNS 重绑定/换配置窗口内可打私网。
func (s *Service) sendWebhook(ctx context.Context, event string, body []byte) error {
	// 取一次当前目标（原子快照）⇒ 后续校验与发送用同一个 URL，
	// 不会因运行期热重建换地址而"校验的是 A、发的�� B"。
	target := s.webhookTarget()
	if target == "" {
		return nil
	}
	if err := security.ValidateEndpointURL(target); err != nil {
		return fmt.Errorf("webhook 目标被拒绝: %w", err)
	}
	return httpjson.DoJSON(ctx, webhookHTTPClient, httpjson.Request{
		Method: http.MethodPost,
		URL:    target,
		Body:   body,
		Header: func(h http.Header) {
			h.Set("Content-Type", "application/json")
			h.Set("X-PB-Event", event)
		},
	}, nil)
}

// EmitShareFlagged 分享命中审核钩子事件（share.flagged）——结构化 webhook 推送，
// 供外挂自动处置（如拉取后调管理端禁用接口）。异步推送，失败静默记日志。
func (s *Service) EmitShareFlagged(code, reason, ownerIP string) {
	if target := s.webhookTarget(); target == "" {
		return
	}
	payload := map[string]interface{}{
		"event":     "share.flagged",
		"code":      code,
		"reason":    reason,
		"owner_ip":  ownerIP,
		"hint":      "share is set pending_review; approve via PUT /admin/files/:id/status",
		"timestamp": time.Now().Unix(),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	async.GoSafe(func() {
		if err := s.sendWebhook(context.Background(), "share.flagged", body); err != nil {
			logger.Warn("webhook push failed", zap.String("url", s.webhookTarget()), zap.Error(err))
		}
	})
}

// EmitP2pInvite 设备直传邀请到达 webhook 推送（P3；best-effort，未配置 webhook 即短路）。
// 邀请面向 admin（收件箱在管理页轮询呈现），此事件供运维侧及时感知。
func (s *Service) EmitP2pInvite(fromName, code, note string) {
	if target := s.webhookTarget(); target == "" {
		return
	}
	payload := map[string]interface{}{
		"event":     "p2p.invite",
		"from_name": fromName,
		"code_hint": code[:4] + "-****-****", // 口令 bearer 不落明文
		"note":      note,
		"hint":      "incoming direct-transfer invite; review via GET /admin/p2p/invites",
		"timestamp": time.Now().Unix(),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	async.GoSafe(func() {
		if err := s.sendWebhook(context.Background(), "p2p.invite", body); err != nil {
			logger.Warn("webhook push failed", zap.String("url", s.webhookTarget()), zap.Error(err))
		}
	})
}

// dispatchWebhook 异步推送（5s 超时，失败静默记日志）
func (s *Service) dispatchWebhook(ctx context.Context, userID uint, title, content, notifyType, level string) {
	if target := s.webhookTarget(); target == "" {
		return
	}
	payload := webhookPayload{
		Event:      "notify.created",
		Title:      title,
		Content:    content,
		NotifyType: notifyType,
		Level:      level,
		UserID:     userID,
		Timestamp:  time.Now().Unix(),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	async.GoSafe(func() {
		// ValueOnlyFrom 剥离取消/超时：请求返回即取消请求 ctx,fire-and-forget
		// webhook 若继承之会在请求尾后被掐断(间歇性推送失败);超时由 client 兜
		if err := s.sendWebhook(contextx.ValueOnlyFrom(ctx), "notify.created", body); err != nil {
			logger.CtxWarn(ctx, "webhook push failed", "url", s.webhookTarget(), "err", err)
		}
	})
}
