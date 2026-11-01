// Package request 寄件码/反向收件（P2）：用户创建投递链接，访客凭链接上传，
// 文件以链接主人名义成为普通分享（复用分享过期/清理/通知机制）。
//
// 设计参照 Gokapi File Requests / Pingvin reverse share：
//   - token 32 hex（crypto/rand），链接 /#/request/<token>
//   - 约束：单次投递文件数/总大小上限、链接有效期（创建者设定）
//   - 访客投递不走 upload.open_upload 总闸（显式受邀上传，与 Gokapi 同策略）
package request

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oekit/kitx/uidgen"
	"github.com/pigeonbox/core/pkg/utils"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/pigeonbox/core/storage"
	"gorm.io/gorm"
)

// 收到投递生成的分享的默认寿命/次数（v1 固定；管理端后续可配）
const (
	defaultRecvDays  = 7
	defaultRecvCount = -1 // 无限次数
)

// 跨站投递槽位（联邦前台化，2026-11-01）的参数。
const (
	// FederationSlotTTL 槽位有效期：铸出后立即用于转发 ⇒ 短 TTL
	//（不留长命窗口；过期后链接自然失效）。
	FederationSlotTTL = 2 * time.Hour
	// federationSlotMaxFiles 单次投递文件数上限（与访客投递端点的硬上限一致）。
	federationSlotMaxFiles = 100
)

// ShareResult 投递落地结果（调用方需要的最小字段）。
type ShareResult struct {
	ID         uint
	Code       string
	PickupCode string // 6 位取件码（未铸造为空）
	Size       int64
}

// ShareCreateRequest 投递转分享的装配参数（网关实现适配为具体分享域的请求）。
type ShareCreateRequest struct {
	Entries      []storage.StoredFileEntry
	OwnerID      uint
	OwnerIP      string
	Channel      string
	ExpiredAt    *time.Time
	ExpiredCount int
}

// ShareGateway 分享创建网关（消费侧接口；bootstrap 注入 share 域适配器，
// 本域不直接依赖 share 包——跨域依赖收口为单一装配点）。
type ShareGateway interface {
	CreateFromFileEntries(ctx context.Context, req *ShareCreateRequest) (*ShareResult, error)
}

// Service 寄件码服务
type Service struct {
	reqRepo  *dao.FileRequestRepository
	shareSvc ShareGateway
	notify   NotifySender
}

// NotifySender 投递到件通知（notify service 实现）
type NotifySender interface {
	CreateForUserSimple(ctx context.Context, userID uint, title, content, notifyType, level string) error
}

// NewService 构建服务。shareSvc 为分享创建网关（未注入时投递创建分享会失败）。
func NewService(gormDB *gorm.DB, shareSvc ShareGateway, notify NotifySender) *Service {
	return &Service{
		reqRepo:  dao.NewFileRequestRepository(gormDB),
		shareSvc: shareSvc,
		notify:   notify,
	}
}

// genToken 32 位 hex 随机令牌（crypto/rand 经 kit/uidgen 统一收口）
func genToken() string {
	return uidgen.RandomHex(16)
}

// CreateReq 创建投递链接入参
type CreateReq struct {
	Title       string
	MaxFiles    int
	MaxBytes    int64
	ExpireValue int
	ExpireStyle string // minute/hour/day/week/month/year/forever；空 = 7 天（不支持 count：该通道按时间过期）
}

// Create 创建投递链接
func (s *Service) Create(ctx context.Context, userID uint, req CreateReq) (*model.FileRequest, error) {
	if req.MaxFiles < 0 || req.MaxBytes < 0 {
		return nil, errors.New("上限不能为负数")
	}
	style := req.ExpireStyle
	if style == "" {
		style = "day"
		value := req.ExpireValue
		if value <= 0 {
			value = defaultRecvDays
		}
		req.ExpireValue = value
	}
	if err := utils.CheckExpireStyleAllowed(style); err != nil {
		return nil, err
	}
	// 「按取件次数」在本通道**不适用**（2026-10-22 审计）：投递码的过期是
	// 按时间算的，取件次数恒为无限（ExpiredCount=defaultRecvCount=-1）。
	// 此前它能通过白名单校验 ⇒ 用户传 count 拿到的是「30 天 + 无限次」，
	// 与「按次数」的期望**静默错配**。现在响亮拒绝，别让人以为限制生效了。
	if style == "count" {
		return nil, errors.New("投递码不支持「按取件次数」过期（该通道只支持按时间过期）")
	}

	fr := &model.FileRequest{
		Token:     genToken(),
		Title:     req.Title,
		UserID:    userID,
		MaxFiles:  req.MaxFiles,
		MaxBytes:  req.MaxBytes,
		ExpiredAt: utils.CalculateExpireTime(req.ExpireValue, style),
	}
	if err := s.reqRepo.Create(ctx, fr); err != nil {
		return nil, err
	}
	return fr, nil
}

// CreateFederationSlot 铸一个「访客投递槽位」（联邦前台化，2026-11-01）。
//
// 语义：对面柜（A 柜）请求在本站（B 柜）铸一个投递槽位，拿到 token 后用它
// 往本站投递文件（POST /api/v1/request/:token/upload）⇒ 落成分享后由**本站**
// 的 share 域铸 6 位取件码 ⇒ **码归属 = 本站**（取件人在本站取件）。
//
// 属主 = 0（无本地用户）⇒ 落成的分享是本站的**匿名分享**（归属天然正确：
// 文件归本站柜子，不冒充某个用户）。
//
// ⚠️ 有效期**不走** Create 的过期样式白名单（utils.CheckExpireStyleAllowed）：
// 槽位是内部通道（不是用户可选的分享过期样式）⇒ 管理员把 allowed_expire_styles
// 收窄成 ["day","week"] 时，铸槽位不该跟着失败。
func (s *Service) CreateFederationSlot(ctx context.Context, title, fromName, note string) (*model.FileRequest, error) {
	expiredAt := time.Now().Add(FederationSlotTTL)
	fr := &model.FileRequest{
		Token:     genToken(),
		Title:     federationSlotTitle(title, fromName, note),
		UserID:    0, // 无本地属主（联邦投递槽位）
		MaxFiles:  federationSlotMaxFiles,
		MaxBytes:  0, // 不限（由本站的存储配额/闸门在落盘时裁决）
		ExpiredAt: &expiredAt,
	}
	if err := s.reqRepo.Create(ctx, fr); err != nil {
		return nil, err
	}
	return fr, nil
}

// federationSlotTitle 拼槽位的展示标题（访客在投递页可见）。
//
// 投递链接模型只有 Title 一个可写字段 ⇒ 发起柜名与留言并进标题（不静默丢弃）。
func federationSlotTitle(title, fromName, note string) string {
	parts := make([]string, 0, 3)
	if v := strings.TrimSpace(title); v != "" {
		parts = append(parts, v)
	}
	if v := strings.TrimSpace(fromName); v != "" {
		parts = append(parts, "来自 "+v)
	}
	if v := strings.TrimSpace(note); v != "" {
		parts = append(parts, v)
	}
	if len(parts) == 0 {
		parts = append(parts, "跨站投递")
	}
	return truncateTitle(strings.Join(parts, " · "), 200)
}

// truncateTitle 按**字符**（不是字节）截断标题（避免切坏多字节 UTF-8）。
func truncateTitle(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if maxRunes <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= maxRunes {
		return s
	}
	return string(rs[:maxRunes])
}

// PublicView 访客侧链接信息
type PublicView struct {
	Token     string     `json:"token"`
	Title     string     `json:"title"`
	MaxFiles  int        `json:"max_files"`
	MaxBytes  int64      `json:"max_bytes"`
	ExpiredAt *time.Time `json:"expired_at"`
}

// GetPublic 访客取链接信息（过期/撤销 → 错误）
func (s *Service) GetPublic(ctx context.Context, token string) (*PublicView, error) {
	fr, err := s.reqRepo.GetByToken(ctx, token)
	if err != nil {
		return nil, errors.New("投递链接不存在或已撤销")
	}
	if fr.IsExpired() {
		return nil, errors.New("投递链接已过期")
	}
	return &PublicView{
		Token:     fr.Token,
		Title:     fr.Title,
		MaxFiles:  fr.MaxFiles,
		MaxBytes:  fr.MaxBytes,
		ExpiredAt: fr.ExpiredAt,
	}, nil
}

// Submit 访客投递（entries 已落存储；此处做约束校验并创建归属分享）。
// 失败时调用方负责清理已落盘文件。
func (s *Service) Submit(ctx context.Context, token string, entries []storage.StoredFileEntry, guestIP string) (*ShareResult, error) {
	fr, err := s.reqRepo.GetByToken(ctx, token)
	if err != nil {
		return nil, errors.New("投递链接不存在或已撤销")
	}
	if fr.IsExpired() {
		return nil, errors.New("投递链接已过期")
	}
	if len(entries) == 0 {
		return nil, errors.New("请选择要投递的文件")
	}
	if fr.MaxFiles > 0 && len(entries) > fr.MaxFiles {
		return nil, fmt.Errorf("单次投递最多 %d 个文件", fr.MaxFiles)
	}
	var total int64
	for _, e := range entries {
		total += e.Size
	}
	if fr.MaxBytes > 0 && total > fr.MaxBytes {
		return nil, fmt.Errorf("单次投递总大小不能超过 %d 字节", fr.MaxBytes)
	}

	ownerID := fr.UserID
	resp, err := s.shareSvc.CreateFromFileEntries(ctx, &ShareCreateRequest{
		Entries:      entries,
		OwnerID:      ownerID,
		OwnerIP:      guestIP,
		Channel:      "request",
		ExpiredAt:    utils.CalculateExpireTime(defaultRecvDays, "day"),
		ExpiredCount: defaultRecvCount,
	})
	if err != nil {
		return nil, err
	}

	// 用量累计 + 通知主人（best-effort）
	_ = s.reqRepo.IncrementUsage(ctx, token, total)
	if s.notify != nil {
		detail := fmt.Sprintf("投递链接: %s\n文件数: %d\n总大小: %d 字节\n投递人 IP: %s\n分享码: %s\n取件码: %s",
			token[:8]+"…", len(entries), total, guestIP, resp.Code, resp.PickupCode)
		_ = s.notify.CreateForUserSimple(ctx, ownerID, "您收到一份新投递", detail, "request_received", "info")
	}
	return resp, nil
}

// ListForUser 用户的投递链接列表
func (s *Service) ListForUser(ctx context.Context, userID uint) ([]*model.FileRequest, error) {
	return s.reqRepo.ListByUserID(ctx, userID)
}

// Delete 撤销链接（仅属主）
func (s *Service) Delete(ctx context.Context, userID uint, token string) (bool, error) {
	return s.reqRepo.Delete(ctx, userID, token)
}
