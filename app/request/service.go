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
	"time"

	"github.com/pigeonbox/core/pkg/utils"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/pigeonbox/core/storage"
	"github.com/oekit/pkg/uidgen"
	"gorm.io/gorm"
)

// 收到投递生成的分享的默认寿命/次数（v1 固定；管理端后续可配）
const (
	defaultRecvDays  = 7
	defaultRecvCount = -1 // 无限次数
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
	ExpireStyle string // day/week/month/...；空 = 7 天
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
