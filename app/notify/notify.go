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
	"net/url"
	"strconv"
	"strings"
	"time"

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

// externalHTTPClient 外部推送共用客户端（webhook / telegram / bark；5s 超时；
// 推送失败静默记日志不重试）。
// 重定向防护（2026-10-05 审计 P3）：webhook URL 仅管理员可配，但保存期校验
// 会被 302 绕过——目标服务器一跳转即可指向私网。CheckRedirect 逐跳复用
// security.ValidateEndpointURL（respect security.ssrf.allow_private_networks），
// 私网目标且未放行时中止。
var externalHTTPClient = &http.Client{
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

	// telegramCfg / barkCfg Telegram 与 Bark 外部推送渠道配置（nil = 禁用；
	// 2026-11-01 外部推送渠道扩展）。
	//
	// ⚠️ 同 webhookURL：运行期热重建写、请求路径读 ⇒ 一律 atomic.Pointer 原子持有。
	telegramCfg atomic.Pointer[TelegramConfig]
	barkCfg     atomic.Pointer[BarkConfig]
}

// webhookTarget 当前生效的 Webhook 地址（空串 = 禁用）。
func (s *Service) webhookTarget() string {
	if p := s.webhookURL.Load(); p != nil {
		return *p
	}
	return ""
}

// telegram / bark 当前生效的渠道配置快照（nil = 禁用）。
func (s *Service) telegram() *TelegramConfig { return s.telegramCfg.Load() }
func (s *Service) bark() *BarkConfig         { return s.barkCfg.Load() }

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

// doPush 执行一次外部推送 POST（fire-and-forget 语义：2xx = 送达，响应体不关心）。
//
// ⚠️ 为什么不直接 httpjson.DoJSON(ctx, client, req, nil)：上游 DoJSON 无条件
// json.Unmarshal(raw, out)——out==nil 时**恒**报 "响应解析失败: json: Unmarshal(nil)"
// （成功推送被误报失败；webhook 渠道自 2026-10 起每次成功送达都记假失败日志，
// 真失败反而淹没其中）。且推送响应体不保证是 JSON（webhook 端点常回纯文本/空）。
//
// 这里以 json.RawMessage 接住响应体，仅把 **2xx 后的响应体解码失败**视为送达；
// 真失败 = 非 2xx（httpjson.StatusError）或传输层/读响应错误。
// 上游缺口（DoJSON 对 out==nil 应跳过解码）见 oekit/pkg issue（PR 注明）。
func doPush(ctx context.Context, req httpjson.Request) error {
	err := httpjson.DoJSON(ctx, externalHTTPClient, req, &json.RawMessage{})
	if err == nil {
		return nil
	}
	var se *httpjson.StatusError
	if errors.As(err, &se) {
		return err // 非 2xx：真失败
	}
	var syn *json.SyntaxError
	if errors.As(err, &syn) {
		return nil // 2xx 已送达，仅响应体非 JSON/为空——与推送语义无关
	}
	return err
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
	return doPush(ctx, httpjson.Request{
		Method: http.MethodPost,
		URL:    target,
		Body:   body,
		Header: func(h http.Header) {
			h.Set("Content-Type", "application/json")
			h.Set("X-PB-Event", event)
		},
	})
}

// EmitShareFlagged 分享命中审核钩子事件（share.flagged）——结构化 webhook 推送 +
// Telegram/Bark 外部渠道广播，供外挂自动处置（如拉取后调管理端禁用接口）。
// 异步推送，失败静默记日志。
func (s *Service) EmitShareFlagged(code, reason, ownerIP string) {
	payload := map[string]interface{}{
		"event":     "share.flagged",
		"code":      code,
		"reason":    reason,
		"owner_ip":  ownerIP,
		"hint":      "share is set pending_review; approve via PUT /admin/files/:id/status",
		"timestamp": time.Now().Unix(),
	}
	title := "分享命中审核: " + code
	content := fmt.Sprintf("原因: %s\n来源 IP: %s\n处置: PUT /admin/files/:id/status", reason, ownerIP)
	s.dispatchExternal(context.Background(), "share.flagged", payload, title, content)
}

// EmitP2pInvite 设备直传邀请到达外部推送（P3；best-effort，未配置任何渠道即短路）。
// 邀请面向 admin（收件箱在管理页轮询呈现），此事件供运维侧及时感知。
func (s *Service) EmitP2pInvite(fromName, code, note string) {
	codeHint := code + "-****-****"
	if len(code) >= 4 {
		codeHint = code[:4] + "-****-****" // 口令 bearer 不落明文
	}
	payload := map[string]interface{}{
		"event":     "p2p.invite",
		"from_name": fromName,
		"code_hint": codeHint,
		"note":      note,
		"hint":      "incoming direct-transfer invite; review via GET /admin/p2p/invites",
		"timestamp": time.Now().Unix(),
	}
	title := "设备直传邀请: " + fromName
	content := fmt.Sprintf("口令提示: %s\n备注: %s\n查看: GET /admin/p2p/invites", codeHint, note)
	s.dispatchExternal(context.Background(), "p2p.invite", payload, title, content)
}

// dispatchWebhook 站内信创建后的外部推送（notify.created）——走 dispatchExternal
// 向 webhook/telegram/bark 全渠道 fan-out（异步 5s 超时，失败静默记日志）。
func (s *Service) dispatchWebhook(ctx context.Context, userID uint, title, content, notifyType, level string) {
	payload := webhookPayload{
		Event:      "notify.created",
		Title:      title,
		Content:    content,
		NotifyType: notifyType,
		Level:      level,
		UserID:     userID,
		Timestamp:  time.Now().Unix(),
	}
	s.dispatchExternal(ctx, "notify.created", payload, title, content)
}

// ==================== Telegram / Bark 外部推送渠道（2026-11-01） ====================
//
// 设计稿：docs/specs/2026-11-01-notify-channels-telegram-bark-design.md（core#31）。
// 渠道抽象是**渐进式 fan-out**（dispatchExternal），不做重量级 Channel 接口重构。

// TelegramConfig Telegram 渠道配置（bot_token + chat_id 均非空 = 启用）。
type TelegramConfig struct {
	BotToken string
	ChatID   string
	// apiBase Bot API 基址（空 = defaultTelegramAPIBase；**仅测试**覆写指向
	// httptest）。host 由代码钉死 api.telegram.org，未导出 ⇒ 用户配置/装配
	// 都改不到它；按配置快照携带（不是可变全局），热重建/并发下无 race。
	apiBase string
}

// BarkConfig Bark 渠道配置（server_url + device_key 均非空 = 启用）。
type BarkConfig struct {
	ServerURL string
	DeviceKey string
}

// defaultTelegramAPIBase Telegram Bot API 基址——目标 host **固定**
// api.telegram.org，不可由用户输入（token/chat_id 是配置值，不构成出站 URL 注入）。
const defaultTelegramAPIBase = "https://api.telegram.org"

// SetTelegram 注入 Telegram 渠道配置（bootstrap 从 notify.telegram 段构建；
// nil 或缺任一字段 = 禁用/卸载）。运行期热重建会调用 ⇒ 原子换指针
// （见 Service.telegramCfg 的说明）。
func (s *Service) SetTelegram(cfg *TelegramConfig) {
	if cfg == nil {
		s.telegramCfg.Store(nil)
		return
	}
	c := *cfg // 整体拷贝（保留 apiBase 测试缝），随后归一化
	c.BotToken = strings.TrimSpace(c.BotToken)
	c.ChatID = strings.TrimSpace(c.ChatID)
	if c.BotToken == "" || c.ChatID == "" {
		s.telegramCfg.Store(nil) // 缺任一字段 = 未启用
		return
	}
	s.telegramCfg.Store(&c)
}

// SetBark 注入 Bark 渠道配置（bootstrap 从 notify.bark 段构建；
// nil 或缺任一字段 = 禁用/卸载）。同 SetTelegram 原子换指针。
func (s *Service) SetBark(cfg *BarkConfig) {
	if cfg == nil {
		s.barkCfg.Store(nil)
		return
	}
	c := BarkConfig{
		ServerURL: strings.TrimSpace(cfg.ServerURL),
		DeviceKey: strings.TrimSpace(cfg.DeviceKey),
	}
	if c.ServerURL == "" || c.DeviceKey == "" {
		s.barkCfg.Store(nil)
		return
	}
	s.barkCfg.Store(&c)
}

// telegramPushURL 拼 Telegram 推送目标（host 固定 api.telegram.org，不可由
// 用户输入；token/chat_id 是配置值，不构成出站 URL 注入）。
func telegramPushURL(cfg *TelegramConfig) string {
	base := cfg.apiBase
	if base == "" {
		base = defaultTelegramAPIBase
	}
	return fmt.Sprintf("%s/bot%s/sendMessage", base, cfg.BotToken)
}

// barkPushURL 拼 Bark 推送目标（server_url + PathEscape(device_key)；
// device_key 走 PathEscape 防路径/query 注入改变目标路径）。
func barkPushURL(cfg *BarkConfig) string {
	return strings.TrimRight(cfg.ServerURL, "/") + "/" + url.PathEscape(cfg.DeviceKey)
}

// telegramText Telegram 消息正文 = title + content（4096 字符硬限，截断好过
// 整条被 Bot API 拒）。
func telegramText(title, content string) string {
	text := title + "\n\n" + content
	if r := []rune(text); len(r) > 4000 {
		text = string(r[:4000])
	}
	return text
}

// sendTelegram 发送 Telegram 消息（POST <base>/bot<token>/sendMessage，
// JSON {chat_id, text}）。
//
// SSRF 纵深防御：host 固定 api.telegram.org，仍对拼好的 URL 复跑
// security.ValidateEndpointURL（防配置期绕过 + DNS 重绑定；沿用 sendWebhook
// 的 2026-10-08 加固范式）。
//
// ⚠️ 错误与日志**不得**携带目标 URL——URL 内嵌 bot token。
func (s *Service) sendTelegram(ctx context.Context, cfg *TelegramConfig, title, content string) error {
	if cfg == nil {
		return nil
	}
	target := telegramPushURL(cfg)
	if err := security.ValidateEndpointURL(target); err != nil {
		return fmt.Errorf("telegram 目标被拒绝: %w", err)
	}
	body, err := json.Marshal(map[string]string{
		"chat_id": cfg.ChatID,
		"text":    telegramText(title, content),
	})
	if err != nil {
		return err
	}
	return doPush(ctx, httpjson.Request{
		Method: http.MethodPost,
		URL:    target,
		Body:   body,
		Header: func(h http.Header) {
			h.Set("Content-Type", "application/json")
		},
	})
}

// sendBark 发送 Bark 推送（POST <server_url>/<device_key>，JSON {title, body}）。
//
// SSRF：ServerURL **由用户填**（自建 Bark）⇒ 发送前必过
// security.ValidateEndpointURL（同 webhook 加固口径），私网目标默认拒
// （沿用全局 security.ssrf 口径，**不为 Bark 开 allow_private 后门**）；
// device_key 走 url.PathEscape，防路径/query 注入改变目标路径。
func (s *Service) sendBark(ctx context.Context, cfg *BarkConfig, title, content string) error {
	if cfg == nil {
		return nil
	}
	target := barkPushURL(cfg)
	if err := security.ValidateEndpointURL(target); err != nil {
		return fmt.Errorf("bark 目标被拒绝: %w", err)
	}
	body, err := json.Marshal(map[string]string{
		"title": title,
		"body":  content,
	})
	if err != nil {
		return err
	}
	return doPush(ctx, httpjson.Request{
		Method: http.MethodPost,
		URL:    target,
		Body:   body,
		Header: func(h http.Header) {
			h.Set("Content-Type", "application/json")
		},
	})
}

// dispatchExternal 外部推送统一出口（2026-11-01 渠道扩展）：向**所有已启用渠道**
// （webhook / telegram / bark）fire-and-forget fan-out。
//
//   - 各渠道**独立 best-effort**：async.GoSafe（禁裸 go），失败只记日志，
//     一渠道失败不影响他渠道，更不影响站内信落库主流程；
//   - ctx 取 contextx.ValueOnlyFrom：剥离取消/超时——fire-and-forget 请求若继承
//     请求 ctx 会在请求尾后被掐断（间歇性推送失败），超时由 client 兜；
//   - 闭包捕获**已 Load 的配置快照**（同 mailer 的注释）：热重建换配置时，
//     本次发送用同一个实例（不会"判空时非 nil、发送时已换掉"）。
//
// SMTP 不并入此 fan-out（它是「对登记邮箱补发」语义，见 CreateForUserSimple）。
func (s *Service) dispatchExternal(ctx context.Context, event string, payload interface{}, title, content string) {
	hookTarget := s.webhookTarget()
	tg := s.telegram()
	br := s.bark()
	if hookTarget == "" && tg == nil && br == nil {
		return // 无任何外部渠道：短路（同此前 dispatchWebhook 未配置即返回）
	}
	var hookBody []byte
	if hookTarget != "" {
		b, err := json.Marshal(payload)
		if err != nil {
			logger.CtxWarn(ctx, "external push payload marshal failed", "event", event, "err", err)
		} else {
			hookBody = b
		}
	}
	// ValueOnlyFrom 剥离取消/超时：请求返回即取消请求 ctx,fire-and-forget
	// 推送若继承之会在请求尾后被掐断(间歇性推送失败);超时由 client 兜
	pushCtx := contextx.ValueOnlyFrom(ctx)
	if hookBody != nil {
		async.GoSafeLabeled("notify-webhook", func() {
			if err := s.sendWebhook(pushCtx, event, hookBody); err != nil {
				logger.CtxWarn(pushCtx, "webhook push failed", "url", hookTarget, "err", err)
			}
		})
	}
	if tg != nil {
		async.GoSafeLabeled("notify-telegram", func() {
			if err := s.sendTelegram(pushCtx, tg, title, content); err != nil {
				// ⚠️ 不打目标 URL：内嵌 bot token
				logger.CtxWarn(pushCtx, "telegram push failed", "event", event, "err", err)
			}
		})
	}
	if br != nil {
		async.GoSafeLabeled("notify-bark", func() {
			if err := s.sendBark(pushCtx, br, title, content); err != nil {
				logger.CtxWarn(pushCtx, "bark push failed", "event", event, "err", err)
			}
		})
	}
}
