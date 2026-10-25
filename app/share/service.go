package share

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	// (async 已不在本文件使用：记取件人改由交付前的计次点触发，见 chargePickup)
	"github.com/oekit/concurrency/retry"
	"github.com/oekit/kitx/uidgen"
	"github.com/pigeonbox/contracts/errcode"
	sharemodel "github.com/pigeonbox/contracts/gen/share"
	"github.com/pigeonbox/core/app/moderation"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/baseurl"
	pkgerrors "github.com/pigeonbox/core/pkg/errors"
	"github.com/pigeonbox/core/pkg/gate"
	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/pkg/metrics"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/pkg/utils"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/pigeonbox/core/storage"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

type ShareTextReq struct {
	Text         string
	ExpiredAt    *time.Time
	ExpiredCount int
	RequireAuth  bool
	PasswordHash string
	UserID       *uint
	UploadType   string
	OwnerIP      string
	Encrypted    bool   // E2E：Text 为客户端密文（P1）
	CustomCode   string // 自定义取件码（P3；登录用户专属，handler 把关）
}

type ShareFileReq struct {
	Channel      string // 上传通道（direct/chunk/presign/anonymous，metrics 用；可空）
	FilePath     string
	Size         int64
	Text         string
	ExpiredAt    *time.Time
	ExpiredCount int
	RequireAuth  bool
	PasswordHash string
	UserID       *uint
	UploadType   string
	OwnerIP      string
	FileHash     string
	IsChunked    bool
	UploadID     string
	Encrypted    bool   // E2E：存储内容为客户端密文（P1）
	CustomCode   string // 自定义取件码（P3；登录用户专属，handler 把关）
	Note         string // 文件备注（可空，取件页展示；2026-10-12 #489）
}

type ShareResp struct {
	ID           uint       `json:"id"`
	Code         string     `json:"code"`
	Prefix       string     `json:"prefix"`
	Suffix       string     `json:"suffix"`
	UUIDFileName string     `json:"uuid_file_name"`
	FilePath     string     `json:"file_path"`
	Size         int64      `json:"size"`
	Text         string     `json:"text"`
	ExpiredAt    *time.Time `json:"expired_at"`
	ExpiredCount int        `json:"expired_count"`
	UsedCount    int        `json:"used_count"`
	FileHash     string     `json:"file_hash"`
	IsChunked    bool       `json:"is_chunked"`
	UploadID     string     `json:"upload_id"`
	UserID       *uint      `json:"user_id"`
	UploadType   string     `json:"upload_type"`
	RequireAuth  bool       `json:"require_auth"`
	Encrypted    bool       `json:"encrypted"` // E2E 客户端加密标记（P1）
	OwnerIP      string     `json:"owner_ip"`
	Status       string     `json:"status"`         // 管控状态（normal/blocked/pending_review）
	ShareURL     string     `json:"share_url"`      // 相对分享链接
	FullShareURL string     `json:"full_share_url"` // 完整分享链接
	// PickupCode 6 位取件码（2026-10-07 起文件分享铸造；永久分享/未注入 minter 时为空。
	// KV 映射仅加速解析，8 位分享码永远可用，故铸造失败不影响主流程）
	PickupCode string `json:"pickup_code,omitempty"`
	// Deduped 秒传去重命中（2026-10-12）：本分享未写入新存储对象，引用调用者
	// 自己历史同 hash+size 文件的既有对象（D1=A 同用户内去重）
	Deduped bool `json:"deduped,omitempty"`
}

// PickupCodeMinter 取件码铸造窄接口（app/anonymous.Service 结构性满足）。
// share 域不 import anonymous 域（域间零耦合），由 bootstrap 注入具体实现。
type PickupCodeMinter interface {
	MintForShare(ctx context.Context, shareCode, fileName string, fileSize int64, requireAuth bool, expireAt *time.Time) (string, error)
}

type Service struct {
	fileCodeRepo *dao.FileCodeRepository
	fileFileRepo *dao.FileCodeFileRepository // 多文件子表 DAO（构造注入）
	userService  UserServiceInterface
	storage      storage.StorageInterface
	baseURL      string // 基础 URL，用于生成分享链接（空=按请求来源动态推断，见 ResolveBase）
	// baseURLProvider 对外 base 地址的**动态**来源（2026-10-22 站点公网地址）。
	// nil = 只用构造期的 baseURL（env/yaml 的 server.base_url）。
	// 非 nil 时每次拼链接都问它 —— 管理后台改「站点公网地址」后**立即生效**，
	// 不必重启、也不必改 .env：多部署方式下这是关键（NAS 用户不碰 .env，
	// 而 server.base_url 只能在 yaml/env 里改，NAS 装完根本不知道它存在）。
	baseURLProvider func(context.Context) string
	notifySvc       NotifyServiceInterface
	quotaChecker    QuotaChecker
	moderator       moderation.Moderator // 内容审核钩子（nil = 不审核）
	flagEmitter     FlagEventEmitter     // share.flagged webhook（nil = 不推送）
	federation      FederationNotifier   // P2P 联邦口令路由钩子（nil = 非联邦模式；读写走 federationMu——P1 后台热切换=运行期写）
	federationMu    sync.RWMutex

	// 存储兜底（storageClient 惰性本地后端，仅未注入时使用；见 files.go）
	fallbackOnce    sync.Once
	fallbackStorage storage.StorageInterface

	// pickupMinter 取件码铸造（bootstrap 注入 anonymous service；nil = 不铸造）
	pickupMinter PickupCodeMinter
}

// 公开链接 base 解析已下沉 pkg/baseurl（Resolve/PublicBaseCtxKey 单源，
// 2026-10-10 消解 presign→share 跨域边时迁出）。

// NotifyServiceInterface 取件通知接口（避免 share → notify 直接依赖）
type NotifyServiceInterface interface {
	CreateForUserSimple(ctx context.Context, userID uint, title, content, notifyType, level string) error
}

// UserServiceInterface 定义用户服务接口，避免循环依赖
type UserServiceInterface interface {
	UpdateUserStats(userID uint, statsType string, value int64) error
	// GetUploadSizeCap 生效的单次上传大小上限（管理员可为单用户降限；0=不限）
	GetUploadSizeCap(ctx context.Context, userID uint) int64
}

// QuotaChecker 存储配额检查接口（bootstrap 注入 user service 实现）
type QuotaChecker interface {
	CheckQuota(ctx context.Context, userID uint, addBytes int64) error
}

// FlagEventEmitter share.flagged 事件推送接口（bootstrap 注入 notify service 实现）
type FlagEventEmitter interface {
	EmitShareFlagged(code, reason, ownerIP string)
}

// FederationNotifier P2P 联邦口令路由钩子（bootstrap 注入；nil = 非联邦模式）。
// 单向可选依赖：本包只定义窄接口，实现在 app/federation（实现方 import 本包，
// 本包不反向依赖——与 NotifyServiceInterface 同风格）。
type FederationNotifier interface {
	// ShareCreated 新分享公告到联邦（实现方内部自滤低熵码，异步不阻塞）。
	ShareCreated(code string, expiresAt *time.Time)
	// ShareDeleted 分享删除时撤销联邦公告（best-effort）。
	ShareDeleted(code string)
}

// SetModerator 注入内容审核钩子（bootstrap 调用；nil = 不审核）
func (s *Service) SetModerator(m moderation.Moderator) { s.moderator = m }

// SetPickupMinter 注入取件码铸造（anonymous 域实现；nil = 文件分享不铸 6 位取件码）
func (s *Service) SetPickupMinter(m PickupCodeMinter) { s.pickupMinter = m }

// SetFlagEventEmitter 注入 share.flagged webhook 推送（bootstrap 调用）
func (s *Service) SetFlagEventEmitter(e FlagEventEmitter) { s.flagEmitter = e }

// SetFederationNotifier 注入 P2P 联邦口令路由钩子（bootstrap 调用；nil = 非联邦。
// P1 后台热切换后为运行期写，与请求路径并发——须加锁）
func (s *Service) SetFederationNotifier(f FederationNotifier) {
	s.federationMu.Lock()
	defer s.federationMu.Unlock()
	s.federation = f
}

// currentFederation 读当前联邦钩子（并发安全快照）
func (s *Service) currentFederation() FederationNotifier {
	s.federationMu.RLock()
	defer s.federationMu.RUnlock()
	return s.federation
}

// federationCreated 联邦公告（nil 安全；实现方保证异步不阻塞建分享）
func (s *Service) federationCreated(code string, expiresAt *time.Time) {
	if n := s.currentFederation(); n != nil {
		n.ShareCreated(code, expiresAt)
	}
}

// federationDeleted 联邦撤销（nil 安全；实现方 best-effort）
func (s *Service) federationDeleted(code string) {
	if n := s.currentFederation(); n != nil {
		n.ShareDeleted(code)
	}
}

// ContentRejectedError 内容未通过审核（moderation 命中且策略为 reject）
type ContentRejectedError struct{}

func (e *ContentRejectedError) Error() string { return "内容未通过安全审核，禁止分享" }

func (e *ContentRejectedError) ErrCode() int { return errcode.CodeContentRejected }

// ShareBlockedError 分享处于管控拒绝态（管理员禁用 / 待审核）。
// handler 侧按 ErrCode 透传（20012 blocked / 20013 pending_review）。
// 实现收口 pkg/errors.ShareBlocked——历史上 anonymous.BlockedError 持有一份
// 逐字相同的拷贝（状态常量还各自漂移），2026-10-10 合并；别名保持域内
// 构造点与 handler errors.As 用法不变。
type ShareBlockedError = pkgerrors.ShareBlocked

// SetShareStatus 管理员设置分享管控状态（禁用/恢复/待审），返回受影响行数。
func (s *Service) SetShareStatus(ctx context.Context, ids []uint, status string) (int64, error) {
	return s.fileCodeRepo.UpdateStatusByIDs(ctx, ids, status)
}

// IsTextShare 判定是否纯文本分享（领域事实已下沉 model.FileCode.IsTextShare；
// 本包装保留给既有调用方，逐步迁移后可删）。
func IsTextShare(fc *model.FileCode) bool {
	return fc.IsTextShare()
}

func NewService(gormDB *gorm.DB, baseURL string, storageService storage.StorageInterface) *Service {
	return &Service{
		fileCodeRepo: dao.NewFileCodeRepository(gormDB),
		fileFileRepo: dao.NewFileCodeFileRepository(gormDB),
		userService:  nil,
		storage:      storageService,
		baseURL:      baseURL,
		notifySvc:    nil,
	}
}

// SetBaseURLProvider 注入对外 base 地址的动态来源（2026-10-22 站点公网地址）。
// 返回空串 = 未配置 → 回退构造期的 baseURL（env/yaml 的 server.base_url），
// 仍为空则由 baseurl.Resolve 按请求来源动态推断（publicBaseMiddleware）。
// 线程安全：装配期注入一次，运行期只读。
func (s *Service) SetBaseURLProvider(fn func(context.Context) string) {
	s.baseURLProvider = fn
}

// resolveBaseURL 取本次请求该用的对外 base（provider 优先，其次构造期值）。
func (s *Service) resolveBaseURL(ctx context.Context) string {
	if s.baseURLProvider != nil {
		if v := strings.TrimSpace(s.baseURLProvider(ctx)); v != "" {
			return v
		}
	}
	return s.baseURL
}

func (s *Service) SetUserService(userService UserServiceInterface) {
	s.userService = userService
}

// SetNotifyService 注入 notify service（取件时给 owner 发通知）
func (s *Service) SetNotifyService(svc NotifyServiceInterface) {
	s.notifySvc = svc
}

// SetQuotaChecker 注入存储配额检查器（bootstrap 注入 user service 实现；
// 未注入时上传不检查配额——保持向后兼容）
func (s *Service) SetQuotaChecker(qc QuotaChecker) {
	s.quotaChecker = qc
}

// checkQuota 登录用户上传前配额检查（匿名上传无配额语义，跳过）
func (s *Service) checkQuota(ctx context.Context, userID *uint, addBytes int64) error {
	if s.quotaChecker == nil || userID == nil {
		return nil
	}
	return s.quotaChecker.CheckQuota(ctx, *userID, addBytes)
}

// validCustomCode 自定义取件码合法性（P3）：3-32 位字母/数字/-/_
func validCustomCode(code string) bool {
	if len(code) < 3 || len(code) > 32 {
		return false
	}
	for _, r := range code {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// createWithCode 带可选自定义码的写库（CustomCode 合法时直接占用；冲突报错不重试；
// 空/非法时回退随机码）。customCode 仅登录用户可指定（handler 侧把关，防匿名抢注）。
func (s *Service) createWithCode(ctx context.Context, customCode string, build func(code string) *model.FileCode) (*model.FileCode, error) {
	// 懒初始化守卫：本方法与 createWithRetry 是仅有的不经公开入口直用
	// s.fileCodeRepo 的写库路径，此前 nil repo 靠零尺寸 struct 的 nil receiver
	// 巧合可跑（dao 注入化改造后必炸，2026-10-10 随改造真修）。
	if customCode == "" {
		return s.createWithRetry(ctx, build)
	}
	if !validCustomCode(customCode) {
		return nil, errors.New("自定义取件码需为 3-32 位字母、数字、- 或 _")
	}
	// 预检占用（glebarez/sqlite 驱动的唯一冲突不映射 gorm.ErrDuplicatedKey，
	// 直接撞唯一索引会漏出裸 SQL 错误；先查再建 + 落库兜底双保险）
	if exists, err := s.fileCodeRepo.CheckCodeExists(ctx, customCode, 0); err == nil && exists {
		return nil, errors.New("自定义取件码已被占用，请换一个")
	}
	// 查询折叠开启时按折叠再查一遍：防 ABC/abc 两条并存后在折叠查询下互撞
	if conf.CodeFoldEnabledOrDefault() {
		if exists, err := s.fileCodeRepo.CheckCodeExistsFolded(ctx, customCode, 0); err == nil && exists {
			return nil, errors.New("自定义取件码已被占用（仅大小写不同），请换一个")
		}
	}
	// 跨列避让（2026-10-23）：自定义码若与某个**已铸取件码**撞同一字符串，
	// 分享域「按码取分享」会优先命中 code 列 ⇒ 那个取件码会打开本分享
	// （错配）。⇒ 自定义码也不能占用取件码已在用的字符串。
	// 取件码字符集全大写，取件解析时输入会被 ToUpper ⇒ 按大写形比对。
	if exists, err := s.fileCodeRepo.CheckPickupCodeExists(ctx, strings.ToUpper(customCode)); err == nil && exists {
		return nil, errors.New("自定义取件码与已有取件码冲突，请换一个")
	}
	fc := build(customCode)
	if err := s.fileCodeRepo.Create(ctx, fc); err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) ||
			strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return nil, errors.New("自定义取件码已被占用，请换一个")
		}
		return nil, err
	}
	return fc, nil
}

// GenerateCode 生成分享代码（8 位字母数字，crypto/rand 经 kit/uidgen 统一收口）。
func (s *Service) GenerateCode() string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	return uidgen.RandomString(charset, 8)
}

// createWithRetry 通用写库重试（code 唯一冲突时换码重试，最多 5 次）。
func (s *Service) createWithRetry(ctx context.Context, build func(code string) *model.FileCode) (*model.FileCode, error) {
	var fc *model.FileCode
	err := retry.Do(ctx, retry.Config{
		Attempts:  5,
		Retryable: func(err error) bool { return errors.Is(err, gorm.ErrDuplicatedKey) },
	}, func(attempt int) error {
		rec := build(s.GenerateCode())
		if err := s.fileCodeRepo.Create(ctx, rec); err != nil {
			return err
		}
		fc = rec
		return nil
	})
	if err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return nil, errors.New("生成分享码失败：多次冲突")
		}
		return nil, err
	}
	return fc, nil
}

// ShareText 分享文本
func (s *Service) ShareText(ctx context.Context, req *ShareTextReq) (*ShareResp, error) {

	fileCode, err := s.createWithCode(ctx, req.CustomCode, func(code string) *model.FileCode {
		return &model.FileCode{
			Code:         code,
			Text:         req.Text,
			ExpiredAt:    req.ExpiredAt,
			ExpiredCount: req.ExpiredCount,
			RequireAuth:  req.RequireAuth,
			PasswordHash: req.PasswordHash,
			UserID:       req.UserID,
			UploadType:   req.UploadType,
			OwnerIP:      req.OwnerIP,
			Encrypted:    req.Encrypted,
		}
	})
	if err != nil {
		return nil, err
	}

	// 更新用户统计（次数 +1；文本分享无存储占用）
	s.bumpUserStats(req.UserID, 0)
	// P2P 联邦公告（未启用为 no-op；实现方自滤低熵码）
	s.federationCreated(fileCode.Code, fileCode.ExpiredAt)

	resp := s.modelToResp(ctx, fileCode)
	// 6 位取件码铸造（2026-10-08 起文本分享与文件分享同权，前端"只保留 6 位码"
	// 呈现）。E2E 密文跳过：解密密钥只随链接传递，凭码取件拿到的只是密文。
	// 非致命——铸造失败仅少一个快捷码，8 位分享码链路不受影响。
	if s.pickupMinter != nil && !req.Encrypted {
		if pickup, err := s.pickupMinter.MintForShare(ctx, fileCode.Code,
			utils.TextPreview(req.Text), int64(len(req.Text)), req.RequireAuth, req.ExpiredAt); err != nil {
			logger.CtxWarn(ctx, "mint pickup code failed (non-fatal)",
				"code", fileCode.Code, "err", err)
		} else {
			resp.PickupCode = pickup
		}
	}
	return resp, nil
}

// textPreview 文本分享的取件码展示名：首行截断 32 rune。取件码 meta 按 '|' 分隔
// 存储（parseMeta），换行与竖线会破坏解析，一律折叠成空格。

// ShareTextWithAuth 带认证的文本分享（用于 Handler）。
// requireAuth=true 时必须提供非空 passwordHash(handler 侧负责哈希),
// 传入空哈希会被拒绝,防止创建出"密码保护形同虚设"的分享。
// encrypted=true 时 text 为客户端密文（P1 E2E），跳过服务端转义与敏感词审核。
func (s *Service) ShareTextWithAuth(ctx context.Context, text string, expireValue int, expireStyle string, requireAuth bool, passwordHash string, userID *uint, ownerIP string, encrypted bool, customCode string) (*ShareResp, error) {
	if requireAuth && passwordHash == "" {
		return nil, errors.New("开启密码保护时必须提供密码")
	}
	// 文本大小上限（service 层兜底，handler 已先行校验）
	if maxBytes := utils.GetTextShareMaxBytes(); maxBytes > 0 && int64(len(text)) > maxBytes {
		return nil, fmt.Errorf("%w（上限 %d 字节）", utils.ErrTextTooLarge, maxBytes)
	}

	// 内容审核钩子（治理 2026-10-03）：reject 建分享前拦截；pending 建分享后置待审。
	// E2E 密文分享跳过（服务端零知识，无明文可审）。
	pendingReview := false
	if !encrypted && s.moderator != nil {
		p, err := s.inspectText(ctx, text)
		if err != nil {
			return nil, err
		}
		pendingReview = p
	}

	// 计算过期时间
	expireTime := utils.CalculateExpireTime(expireValue, expireStyle)
	expireCount := utils.CalculateExpireCount(expireStyle, expireValue)

	uploadType := "anonymous"
	if userID != nil {
		uploadType = "authenticated"
	}

	req := &ShareTextReq{
		Text:         text,
		ExpiredAt:    expireTime,
		ExpiredCount: expireCount,
		RequireAuth:  requireAuth,
		PasswordHash: passwordHash,
		UserID:       userID,
		UploadType:   uploadType,
		OwnerIP:      ownerIP,
		Encrypted:    encrypted,
		CustomCode:   customCode, // 登录用户自定义取件码（匿名传空）
	}

	resp, err := s.ShareText(ctx, req)
	if err != nil {
		return nil, err
	}

	// pending 策略：建分享成功后置 pending_review（取件路径拒绝），并推 share.flagged。
	// fail-closed：置待审失败时回退为 blocked——审核拦截宁可误禁也不可静默放行。
	if pendingReview {
		status, err := s.applyPending(ctx, resp.ID, resp.Code, "text sensitive word", ownerIP)
		if err != nil {
			return nil, err
		}
		resp.Status = status // resp 是写库前快照，回填给调用方
	}

	// 生成分享 URL（页面链接 = hash 路由形态 {base}/#/share/{code}，
	// 路径形态是死链——见 pkg/baseurl/shareurl.go 的形态约定）
	resp.ShareURL = baseurl.SharePageURL("", resp.Code)
	resp.FullShareURL = baseurl.SharePageURL(baseurl.Resolve(ctx, s.resolveBaseURL(ctx)), resp.Code)

	return resp, nil
}

// ============ 三通道共用私有逻辑（文本 / 单文件 / 多文件） ============

// checkUploadCaps 单用户单次上传上限（users.max_upload_size，0=不限；匿名无此
// 约束）+ 站点级全局存储配额 + 存储配额强制执行。超限返回带具体上限文案的错误。
func (s *Service) checkUploadCaps(ctx context.Context, userID *uint, totalSize int64) error {
	// 站点级全局存储配额（storage.quota，0=不限）：全通道统一闸口
	// （直传/分片完成/预签名完成/本地导入/多文件均经此处）。统计口径=存活
	// file_codes 尺寸合计（软删除不计；未完成分片会话暂不计入）。统计故障时
	// fail-open（配额是治理项而非安全项，不因统计异常拒绝所有上传）。
	if cfg := conf.GetGlobalConfig(); cfg != nil && cfg.Storage.Quota > 0 {
		quota := cfg.Storage.Quota
		if used, err := s.fileCodeRepo.GetTotalSize(ctx); err == nil {
			if used+totalSize > quota {
				metrics.RecordRejected(metrics.RejectQuota)
				return &GlobalQuotaExceededError{Used: used, Quota: quota}
			}
		} else {
			logger.CtxWarn(ctx, "全局存储配额统计失败，本次跳过配额检查", "err", err)
		}
	}
	if userID != nil && s.userService != nil {
		if capSize := s.userService.GetUploadSizeCap(ctx, *userID); capSize > 0 {
			// typed error（CodeTooLarge→413）：此前为普通 error，NewTypedError
			// 兜底 CodeInternal，前端显示「服务器内部错误」掩盖超限真相（2026-10-19 事故）。
			if err := gate.CheckUploadSizeCap(userID, totalSize, capSize); err != nil {
				return err
			}
		}
	}
	if err := s.checkQuota(ctx, userID, totalSize); err != nil {
		metrics.RecordRejected(metrics.RejectQuota)
		return err
	}
	return nil
}

// CheckUploadSizeCap 登录用户单次上传总大小预检（多文件绑定按申报大小 fail-fast，
// 与匿名配额预检对称，先于合并落盘；匿名用户返回 nil——其约束走匿名日配额）。
func (s *Service) CheckUploadSizeCap(ctx context.Context, userID uint, totalSize int64) error {
	if s.userService == nil {
		return nil
	}
	return gate.CheckUploadSizeCap(&userID, totalSize, s.userService.GetUploadSizeCap(ctx, userID))
}

// GlobalQuotaExceededError 站点级存储配额超限（handler 侧按业务码透传 CodeStorageQuota）。
type GlobalQuotaExceededError struct {
	Used  int64
	Quota int64
}

func (e *GlobalQuotaExceededError) Error() string {
	return fmt.Sprintf("站点存储空间已满（已用 %d / 上限 %d 字节），请联系管理员清理过期分享或调整配额", e.Used, e.Quota)
}

func (e *GlobalQuotaExceededError) ErrCode() int { return errcode.CodeStorageQuota }

// bumpUserStats 用户统计双写：次数 +1；size>0 时加存储。best-effort（失败只记日志）。
func (s *Service) bumpUserStats(userID *uint, size int64) {
	if s.userService == nil || userID == nil {
		return
	}
	if err := s.userService.UpdateUserStats(*userID, "uploads", 1); err != nil {
		logger.Warn("update user uploads stat failed", zap.Error(err), zap.Uint("user_id", *userID))
	}
	if size != 0 {
		if err := s.userService.UpdateUserStats(*userID, "storage", size); err != nil {
			logger.Warn("update user storage stat failed", zap.Error(err), zap.Uint("user_id", *userID))
		}
	}
}

// inspectText 文本审核判定：reject → ContentRejectedError；pending → true。
func (s *Service) inspectText(ctx context.Context, text string) (bool, error) {
	switch s.moderator.InspectText(ctx, text) {
	case moderation.VerdictReject:
		metrics.RecordModerationHit("reject")
		metrics.RecordRejected(metrics.RejectModerated)
		return false, &ContentRejectedError{}
	case moderation.VerdictPending:
		metrics.RecordModerationHit("pending")
		return true, nil
	}
	return false, nil
}

// inspectFile 单文件审核判定（语义同 inspectText；meta 由各通道自组）。
func (s *Service) inspectFile(ctx context.Context, meta moderation.UploadMeta) (bool, error) {
	switch s.moderator.InspectFile(ctx, meta) {
	case moderation.VerdictReject:
		metrics.RecordModerationHit("reject")
		metrics.RecordRejected(metrics.RejectModerated)
		return false, &ContentRejectedError{}
	case moderation.VerdictPending:
		metrics.RecordModerationHit("pending")
		return true, nil
	}
	return false, nil
}

// applyPending pending 处置（fail-closed，三通道共用）：置 pending_review 失败
// 回退 blocked；回退也失败删记录并拒绝——审核拦截宁可误禁也不可静默放行。
// 成功返回最终 status（供调用方回填响应快照）。
func (s *Service) applyPending(ctx context.Context, id uint, code, reason, ownerIP string) (string, error) {
	target := model.StatusPendingReview
	if _, err := s.SetShareStatus(ctx, []uint{id}, target); err != nil {
		logger.CtxWarn(ctx, "set pending_review failed, fallback to blocked", "code", code, "err", err)
		target = model.StatusBlocked
		if _, berr := s.SetShareStatus(ctx, []uint{id}, target); berr != nil {
			logger.CtxError(ctx, "pending fallback blocked failed, removing share", "code", code, "err", berr)
			_ = s.fileFileRepo.SoftDeleteByFileCodeIDs(ctx, []uint{id}) // 文本分享无子行，no-op
			_ = s.fileCodeRepo.Delete(ctx, id)
			return "", &ContentRejectedError{}
		}
	}
	if s.flagEmitter != nil {
		s.flagEmitter.EmitShareFlagged(code, reason, ownerIP)
	}
	return target, nil
}

// ShareFile 分享文件
func (s *Service) ShareFile(ctx context.Context, req *ShareFileReq) (*ShareResp, error) {
	return s.CreateShare(ctx, req)
}

// CreateShare 创建分享记录（ShareFile 的语义化别名，便于其他 service 调用）
// 行为：单用户上传上限检查 → 存储配额检查 → 生成 code → 写 file_codes 表 → 返回 share_code / url
func (s *Service) CreateShare(ctx context.Context, req *ShareFileReq) (*ShareResp, error) {

	// 单用户单次上传上限 + 存储配额强制执行
	if err := s.checkUploadCaps(ctx, req.UserID, req.Size); err != nil {
		return nil, err
	}

	// 秒传去重（2026-10-12，D1=A 同用户内）：命中自己历史同 hash+size 文件时，
	// 回收冗余副本、新分享引用源对象（多分享共享物理文件——物理删除前必须查
	// 引用，见 CountFilePathReferences 消费点）。
	// ⚠️ 冗余副本回收必须走引用保护而非裸删：presign/chunk 秒传建分享传入的
	// req.FilePath 本身可能就是某个既有源的路径（被其分享行引用），两查之间
	// 若有更新的源出现而发生路径切换，裸删会误删既有源的物理文件（2026-10-12
	// 实施中自查发现）；引用保护下被引用的路径只会跳过回收，多留的孤儿由
	// janitor 对账兜底。
	deduped := false
	if req.FileHash != "" && req.UserID != nil && req.FilePath != "" {
		if src, derr := s.fileCodeRepo.FindQuickUploadSource(ctx, *req.UserID, req.FileHash, req.Size); derr == nil && src != nil {
			if srcPath := src.GetFilePath(); srcPath != "" && srcPath != req.FilePath {
				s.deletePhysicalIfUnreferenced(ctx, req.FilePath, 0)
				req.FilePath = srcPath
				deduped = true
			}
		}
	}

	// 文件审核钩子（治理）：直传/分片/秒传三通道共用本入口。
	// reject 建分享前拦截；pending 建分享后置待审并推事件（与文本通道同策略）。
	filePending := false
	if s.moderator != nil {
		meta := moderation.UploadMeta{
			FileName:    req.Text, // 直传/分片把原始文件名存 Text
			Size:        req.Size,
			OwnerIP:     req.OwnerIP,
			UserID:      req.UserID,
			UploadID:    req.UploadID,
			Channel:     req.Channel,
			StoragePath: req.FilePath, // 文件内容扫描（ClamAV）读取路径
		}
		p, err := s.inspectFile(ctx, meta)
		if err != nil {
			return nil, err
		}
		filePending = p
	}

	fileCode, err := s.createWithCode(ctx, req.CustomCode, func(code string) *model.FileCode {
		return &model.FileCode{
			Code:         code,
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
			IsChunked:    req.IsChunked,
			UploadID:     req.UploadID,
			Encrypted:    req.Encrypted,
			Note:         req.Note,
		}
	})
	if err != nil {
		return nil, err
	}

	// 子表同步写一行（P0 多文件：新旧分享统一经子表读取；失败仅降级为主表读取，不阻断）
	if fc := fileCode; fc != nil {
		if rel := fc.GetFilePath(); rel != "" {
			child := &model.FileCodeFile{
				FileCodeID:   fc.ID,
				FileName:     req.Text,
				UUIDFileName: fc.UUIDFileName,
				FilePath:     rel,
				Size:         req.Size,
				FileHash:     req.FileHash,
			}
			if err := s.fileFileRepo.CreateBatch(ctx, []*model.FileCodeFile{child}); err != nil {
				logger.CtxWarn(ctx, "write child file row failed (fallback to legacy fields)",
					"code", fc.Code, "err", err)
			}
		}
	}

	// 更新用户统计（次数 +1，存储按文件大小）
	s.bumpUserStats(req.UserID, req.Size)

	// 文件 pending 策略（fail-closed 同文本通道）
	if filePending {
		if _, err := s.applyPending(ctx, fileCode.ID, fileCode.Code, "file flagged by moderator", req.OwnerIP); err != nil {
			return nil, err
		}
	}

	// 业务 metrics（治理）：channel 缺省按 upload_type 记
	channel := req.Channel
	if channel == "" {
		channel = req.UploadType
	}
	metrics.RecordUploadBytes(channel, req.Size)
	metrics.RecordShareCreated(req.UploadType)
	// P2P 联邦公告（未启用为 no-op）
	s.federationCreated(fileCode.Code, fileCode.ExpiredAt)

	resp := s.modelToResp(ctx, fileCode)
	resp.Deduped = deduped
	// 6 位取件码铸造（2026-10-07）：非致命——失败仅少一个快捷码，8 位分享码不受影响
	if s.pickupMinter != nil {
		if pickup, err := s.pickupMinter.MintForShare(ctx, fileCode.Code, req.Text, req.Size, req.RequireAuth, req.ExpiredAt); err != nil {
			logger.CtxWarn(ctx, "mint pickup code failed (non-fatal)",
				"code", fileCode.Code, "err", err)
		} else {
			resp.PickupCode = pickup
		}
	}
	return resp, nil
}

// GetFileByCode 通过代码获取文件（**分享码或 6 位取件码**）。
//
// 解析规则见 dao.ResolveByCode（按码解析的唯一真相源）：6 位取件码优先（真相源
// file_codes.pickup_code 列），否则分享码/自定义码（精确 → 大小写折叠）。
// 本域与预览域/匿名域/MCP 共用该解析器 ⇒ 同一串码处处解析到同一个分享。
//
// 取件码能被解析 ⇒ 落地后 `{base}/#/share/{取件码}` 可直接打开取件页
// （此前取件码只能走匿名通道，没有页面链接）。
//
// 为什么安全：① 取件码的枚举面本来就有（/anonymous/search 早能按取件码查，且与
// 本面同属 download 限流桶：per-IP 50 QPS / burst 20）⇒ 本改动不新增枚举面；
// 取件码空间 32^6 ≈ 10.7 亿（字符集 ABCDEFGHJKMNPQRSTUVWXYZ23456789）。
// ⚠️ 精确点：分享域的**失败锁定**（per IP+code）只记密码错，**不存在的码不记
// 失败** ⇒ 枚举的防线是**限流**，不是锁定。
// ② 取件码本就是「口令语义」的取件凭证（拿到码即可取件，API 指南 §6 明写）
// ⇒ 多一个入口不扩大信息面；
// ③ 取件码列是 DB 真相源，不依赖匿名域的 Redis 映射 ⇒ 映射过期/重启不失联；
// ④ 铸造取件码与创建自定义分享码两侧都做了**跨列避让**
// （app/anonymous.GenerateCode / 本文件 createWithCode）⇒ 两种码不会互相串。
func (s *Service) GetFileByCode(ctx context.Context, code string) (*model.FileCode, error) {
	fileCode, err := s.fileCodeRepo.ResolveByCode(ctx, code, conf.CodeFoldEnabledOrDefault())
	if err != nil {
		return nil, err
	}
	return s.validateShareRecord(fileCode)
}

// validateShareRecord 过期 / 管控判定（各解析路径共用）。
func (s *Service) validateShareRecord(fileCode *model.FileCode) (*model.FileCode, error) {
	// 检查文件是否过期
	if fileCode.IsExpired() {
		return nil, errors.New("file has expired")
	}

	// 管控状态：blocked / pending_review 拒绝取件（typed error，handler 按业务码透传）
	if fileCode.IsBlockedShare() {
		metrics.RecordRejected(metrics.RejectBlocked)
		return nil, &ShareBlockedError{Status: fileCode.Status}
	}

	return fileCode, nil
}

// GetFilesByUserID 获取用户的文件列表
func (s *Service) GetFilesByUserID(ctx context.Context, userID uint, page, pageSize int) ([]*model.FileCode, int64, error) {
	return s.fileCodeRepo.GetFilesByUserIDWithPagination(ctx, userID, page, pageSize)
}

// DeleteFile 删除文件
func (s *Service) DeleteFile(ctx context.Context, fileID uint, userID *uint) error {

	// 联邦撤销需要口令,先查（best-effort:查不到跳过钩子,不影响删除本身）
	var fedCode string
	if file, err := s.fileCodeRepo.GetByID(ctx, fileID); err == nil {
		fedCode = file.Code
	}

	// 如果指定了用户ID，验证文件所有权
	if userID != nil {
		file, err := s.fileCodeRepo.GetByUserID(ctx, *userID, fileID)
		if err != nil {
			return err
		}

		// 更新用户统计（减少存储空间）
		if s.userService != nil && userID != nil {
			if err := s.userService.UpdateUserStats(*userID, "storage", -file.Size); err != nil {
				logger.CtxWarn(ctx, "update user storage stat failed on delete", "err", err, "user_id", *userID)
			}
		}
	}

	if err := s.fileCodeRepo.Delete(ctx, fileID); err != nil {
		return err
	}
	s.federationDeleted(fedCode)
	return nil
}

// DeleteFileByCode 根据分享码删除文件。
//
// 删除顺序（保证一致性）：
//  1. 查所有权
//  2. 删 DB 记录（事务原子）
//  3. 事务提交后删物理文件（不可回滚，失败记日志不阻断）
//  4. 扣减用户 storage 统计（best-effort，失败记日志）
//
// 关键：先删 DB 再删物理，避免"物理删了但 DB 没删"的孤儿反向场景。
func (s *Service) DeleteFileByCode(ctx context.Context, code string, userID uint) error {

	// 1. 根据 code 查询文件记录
	file, err := s.fileCodeRepo.GetByCode(ctx, code)
	if err != nil {
		return fmt.Errorf("分享不存在")
	}

	// 2. 验证文件所有权（userID匹配）
	if file.UserID == nil || *file.UserID != userID {
		return fmt.Errorf("无权限删除此分享")
	}

	// 3. 删除数据库记录（事务）
	if err := s.fileCodeRepo.DeleteByIDTx(ctx, file.ID); err != nil {
		return fmt.Errorf("删除分享记录失败: %w", err)
	}

	// 4. 事务提交成功后，删除物理文件（不可回滚，放事务外，失败记日志不阻断）
	//    多文件分享：子表各文件一并删除（legacy 单文件走主表路径）；
	//    秒传共享对象保护（2026-10-12）：删前查引用，被其他分享共享时保留
	if s.storage != nil {
		s.deletePhysicalChildren(ctx, file.ID)
		if file.FilePath != "" && file.UUIDFileName != "" {
			s.deletePhysicalIfUnreferenced(ctx, file.GetFilePath(), file.ID)
		}
	}

	// 5. 扣减用户 storage 统计（best-effort，失败记日志）
	if s.userService != nil {
		if err := s.userService.UpdateUserStats(userID, "storage", -file.Size); err != nil {
			logger.CtxWarn(ctx, "update user storage stat failed on delete",
				"user_id", userID, "size", file.Size, "err", err)
		}
	}

	// 6. 联邦撤销公告（未启用为 no-op）
	s.federationDeleted(code)

	return nil
}

// GetFileList 已删除：全库无调用方的 List 薄包装（管理端列表走 admin.GetFiles，
// 用户列表走 ListUserShares）。

// UpdateFileUsage 原子扣减剩余次数（DB 为准，防并发超卖）。
// 返回 ok=true 表示扣减成功（可下载）；ok=false 表示已耗尽。
//
// ⚠️ 2026-10-24 起**下载链路不再直接调它**：扣次改由 ChargePickup 按「取件会话
// （下载令牌）」执行（一个令牌至多扣 1 次 ⇒ 多文件逐个下载 / 断点续传 / 重复点击
// 不再重复吃次数）。本方法保留给「无令牌会话概念」的其它调用面。
func (s *Service) UpdateFileUsage(ctx context.Context, code string) (bool, error) {
	return s.fileCodeRepo.DecrementExpiredCount(ctx, code)
}

// ChargePickup 按「取件会话」计次（2026-10-24）。
//
// tokenFingerprint = 下载令牌的计次指纹（pkg/security 校验时给出；令牌未启用
// 或未携带时为空 ⇒ 无会话概念，按「每次交付都算一次取件」处理）。
// viewerIP = 取件人 IP（可信代理解析后）⇒ 随计次记录落库，供 owner 侧逐条展示。
//
// 返回：
//   - charged=true：本次交付**真正扣了**取件次数（该令牌的首次使用）⇒ 调用方
//     应在此记取件人 + 通知 owner；
//   - exhausted=true：取件次数已用完且本令牌尚未计次 ⇒ 拒绝交付（20004）；
//   - charged=false, exhausted=false：同一令牌已计次（同一取件会话内的后续
//     交付：多文件逐个下载 / 断续传 / 重复点击）⇒ 不再扣次、也不再记取件人。
func (s *Service) ChargePickup(ctx context.Context, code, tokenFingerprint, channel, viewerIP string) (charged bool, exhausted bool, err error) {
	return s.fileCodeRepo.ChargePickupOnce(ctx, code, tokenFingerprint, channel, viewerIP)
}

// HasPickupCharge 判「该取件会话是否已计次」（取件页在额度用完时的放行判定：
// 已经取过的人仍可再打开，不重复扣次）。
func (s *Service) HasPickupCharge(ctx context.Context, code, sessionKey string) (bool, error) {
	return s.fileCodeRepo.HasPickupCharge(ctx, code, sessionKey)
}

// FindShareByUploadID 按上传会话 ID 回读已建的分享（**上传完成面的幂等键**）。
//
// 背景（2026-10-24 审计，core v0.32.6）：chunk/presign 的 Complete 此前不检查
// 是否已建过分享 ⇒ 同一 upload_id 二次 Complete 会**再建一条分享**（各有独立
// 取件额度 ⇒ 「最多取 N 次」实际可取 2N 次；真机已出现 1 组重复数据）。
//
// ⇒ 完成面先回读：命中即幂等返回（同一个分享码），不新建行、不重复扣次、
// 不重复计配额。未命中返回 (nil, nil)（调用方按「首次」继续建）。
func (s *Service) FindShareByUploadID(ctx context.Context, uploadID string) (*model.FileCode, error) {
	if uploadID == "" {
		return nil, nil
	}
	fc, err := s.fileCodeRepo.FirstByUploadID(ctx, uploadID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return fc, nil
}

// ListPickups 取某分享的逐条取件明细（owner 侧「取件历史」用）。
//
// 数据源 = 计次记录表（一次取件 = 一行）⇒ 多文件逐个下载/断点续传不会被
// 显示成「取件 N 次」（那正是此前只有「最后 IP + 计数」时看不出来的失真）。
func (s *Service) ListPickups(ctx context.Context, code string, limit int) ([]model.PickupCharge, error) {
	return s.fileCodeRepo.ListPickups(ctx, code, limit)
}

// ErrShareNotReady 分享未完成上传（占位记录：文件还没传上来）。
//
// 与匿名通道 app/anonymous 的 ErrNotReady 同一语义 ⇒ 两个面对同一份占位分享
// 给出同一个业务码（20001）与文案，不再是"分享域 200 + 一条指向不存在文件的
// 下载直链"、匿名通道 20001 的口径分叉（2026-10-23 审计）。
var ErrShareNotReady = errors.New("分享未完成上传，暂时无法取件")

// ErrShareCountExhausted 取件次数已用完（2026-10-24：次数耗尽从 IsExpired 里
// 拆出后，「耗尽」不再混作「过期/不存在」，各面给独立错误码
// —— 20004 CodeShareReachLimit / 匿名通道 20007 CodePickupCodeExhausted）。
//
// 本哨兵供 app/share 的元数据面（GetShareMetadata）区分「次数用完」与
// 「不存在/已过期/被封禁」⇒ handler 据此回 20004 而不是笼统的 404。
var ErrShareCountExhausted = errors.New("取件次数已用完")

// IsShareNotReady 判定记录是否为"未完成上传"的占位分享。
// 判定式在 model（唯一真相源，preview/select/download/metadata 共用）。
func IsShareNotReady(fc *model.FileCode) bool { return model.IsNotReady(fc) }

// GetFileWithUsage 获取文件并校验密码（**不扣次数、不记取件人**）。
//
// 扣次与记取件人由调用方在**内容交付前**统一执行（ChargePickup + 记 viewer）：
// ① 扣次必须与「交付」同点（交付失败不该白吃次数）；
// ② 记取件人必须与扣次同点（同一次取件只记一个取件人，重复下载不刷列表）。
//
// viewerIP 由 handler 从可信解析 middleware.ClientIP(c) 注入。
// authedByToken：调用方已校验有效下载令牌（令牌由取件查询在密码/取件校验通过后
// 签发，等价于已认证——否则密码保护分享"凭令牌下载"还要再输一次密码，令牌失效）。
func (s *Service) GetFileWithUsage(ctx context.Context, code, password, viewerIP string, authedByToken bool) (*model.FileCode, error) {

	fileCode, err := s.GetFileByCode(ctx, code)
	if err != nil {
		return nil, err
	}
	// 占位分享（文件未上传）：直接拒，不扣次数、不记取件人
	// （此前会先扣次数再在下载时 500 —— 匿名通道早已用 ErrNotReady 挡住）。
	if IsShareNotReady(fileCode) {
		return nil, ErrShareNotReady
	}

	// 真实密码校验 + 防爆破锁定（统一取件闸门 pkg/gate.CheckPickup）；持有效
	// 下载令牌视为已认证。锁定与 select/匿名取件/preview 面共享 scope "pickup"
	// 的 (IP, code) 失败计数——此前锁定在 handler 层且独立 scope "download"。
	// 历史脏数据（require_auth=true 而哈希缺失）与密码错误一律按「密码错误」
	// 拒绝并记账（旧「配置异常」专属文案会被 handler 兜底分支吞成 404，
	// 实际到不了客户端，收口后统一为 401 密码错误语义）。
	if fileCode.RequireAuth && !authedByToken {
		decision := gate.CheckPickup(ctx, gate.PickupState{
			RequireAuth:  true,
			PasswordHash: fileCode.PasswordHash,
		}, gate.PickupCheck{
			ClientIP: viewerIP,
			// 失败记账用**规范分享码**（6 位取件码解析后）⇒ 与 select 面同一把
			// 钥匙 ⇒ 同一分享的两种入口共享爆破预算（2026-10-23 审计）。
			Code:                 fileCode.Code,
			Password:             password,
			MissingCountsFailure: true, // 保持本通道历史规格：空密码按失败记账
		})
		switch decision.Verdict {
		case gate.PickupLocked:
			return nil, &middleware.LockedError{RemainingSeconds: decision.LockRemain}
		case gate.PickupPasswordMissing, gate.PickupPasswordWrong:
			return nil, errors.New("密码错误")
		}
	}

	return fileCode, nil
}

// RecordViewerAndNotify 记录取件人 + 给 owner 发通知。
//   - viewerIP: 取件人 IP（由 handler 从可信解析 middleware.ClientIP(c) 注入）
//   - viewerDetail: 额外通知内容
//
// 通知去重：同 code 5 分钟内只通知一次（用 LastNotifiedAt 字段）。
// 若 file_codes 找不到，静默跳过。
func (s *Service) RecordViewerAndNotify(ctx context.Context, code, viewerIP, viewerDetail string) error {
	if err := s.fileCodeRepo.UpdateViewer(ctx, code, viewerIP); err != nil {
		// 文件可能不存在，记日志后跳过
		logger.CtxWarn(ctx, "update viewer failed", "code", code, "err", err)
		return nil
	}
	// 读最新记录判断 owner
	fc, err := s.fileCodeRepo.GetByCode(ctx, code)
	if err != nil || fc == nil {
		return nil
	}
	if fc.UserID == nil || s.notifySvc == nil {
		return nil
	}
	// 通知去重：同 code 5 分钟内只通知一次
	if fc.LastNotifiedAt != nil && time.Since(*fc.LastNotifiedAt) < 5*time.Minute {
		return nil
	}
	// 更新 LastNotifiedAt（先更新，避免并发重复通知）
	now := time.Now()
	if err := s.fileCodeRepo.UpdateColumns(ctx, fc.ID, map[string]interface{}{"last_notified_at": now}); err != nil {
		logger.CtxWarn(ctx, "update last_notified_at failed", "id", fc.ID, "err", err)
	}

	title := "您的分享已被取件"
	if IsTextShare(fc) {
		title = "您的文本分享已被查看"
	}
	content := fmt.Sprintf("分享码: %s\n取件人 IP: %s\n时间: %s", code, viewerIP, now.Format("2006-01-02 15:04:05"))
	if viewerDetail != "" {
		content += "\n" + viewerDetail
	}
	return s.notifySvc.CreateForUserSimple(ctx, *fc.UserID, title, content, "share_retrieved", "info")
}

// modelToResp 将模型转换为响应
func (s *Service) modelToResp(ctx context.Context, fileCode *model.FileCode) *ShareResp {
	status := fileCode.Status
	if status == "" {
		status = model.StatusNormal
	}
	return &ShareResp{
		ID:           fileCode.ID,
		Code:         fileCode.Code,
		Status:       status,
		Prefix:       fileCode.Prefix,
		Suffix:       fileCode.Suffix,
		UUIDFileName: fileCode.UUIDFileName,
		FilePath:     fileCode.FilePath,
		Size:         fileCode.Size,
		Text:         fileCode.Text,
		ExpiredAt:    fileCode.ExpiredAt,
		ExpiredCount: fileCode.ExpiredCount,
		UsedCount:    fileCode.UsedCount,
		FileHash:     fileCode.FileHash,
		IsChunked:    fileCode.IsChunked,
		UploadID:     fileCode.UploadID,
		UserID:       fileCode.UserID,
		UploadType:   fileCode.UploadType,
		RequireAuth:  fileCode.RequireAuth,
		Encrypted:    fileCode.Encrypted,
		OwnerIP:      fileCode.OwnerIP,
		// 分享链接三通道统一在此生成（页面链接 = hash 路由形态，见 pkg/baseurl/shareurl.go；
		// 文本通道尾部原有一份同值覆盖，保持无害）
		ShareURL:     baseurl.SharePageURL("", fileCode.Code),
		FullShareURL: baseurl.SharePageURL(baseurl.Resolve(ctx, s.resolveBaseURL(ctx)), fileCode.Code),
	}
}

// UserShareListItem 已删除（2026-10-09 IDL 化）：列表项 wire 契约收敛为
// contracts 生成模型 sharemodel.UserShareItemData（idl/share.thrift
// UserShareItemData），此前 Go/TS 双手抄在 pickup_code 变更时被迫三处同步。

// pickupDetailLimit 每条分享随列表返回的取件明细条数上限（owner 侧「取件历史」）。
// 明细是增强信息：超出的旧记录可在管理端传输日志里看全量。
const pickupDetailLimit = 20

// deletedAtToPtr gorm.DeletedAt → *time.Time（nil 表示未删除）
func deletedAtToPtr(d gorm.DeletedAt) *time.Time {
	if d.Valid {
		return &d.Time
	}
	return nil
}

// ListUserShares 获取用户的分享列表（带筛选）。
// status 取值：all/active/expired/text/file/deleted/viewed；search 模糊匹配 code/文件名/取件码。
// 筛选条件收散参由本域转换，transport 无需感知 dao.UserShareFilter。
// 返回契约生成模型（wire 形态由 idl/share.thrift 定义，handler 只组信封）。
func (s *Service) ListUserShares(ctx context.Context, userID uint, status, search string, page, pageSize int) ([]*sharemodel.UserShareItemData, int64, error) {
	files, total, err := s.fileCodeRepo.GetUserSharesWithFilter(ctx, userID, dao.UserShareFilter{
		Status:   status,
		Search:   search,
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		return nil, 0, err
	}
	items := make([]*sharemodel.UserShareItemData, 0, len(files))
	// 批量取子文件数（P0 多文件；一次 GROUP BY，避免列表页 N+1）
	ids := make([]uint, 0, len(files))
	for _, f := range files {
		ids = append(ids, f.ID)
	}
	counts, cerr := s.fileFileRepo.CountByFileCodeIDs(ctx, ids)
	if cerr != nil {
		counts = nil // 计数失败降级为 0，不阻断列表
	}
	for _, f := range files {
		item := toUserShareItemData(f)
		if counts != nil {
			item.FileCount = int32(counts[f.ID])
		}
		// 逐条取件明细（2026-10-24）：owner 侧「取件历史」的数据源。
		// 一次取件 = 一行 ⇒ 多文件逐个下载/断点续传不会被显示成「取件 N 次」。
		// 取不到明细不阻断列表（明细是增强信息，不是列表的必需项）。
		if rows, perr := s.fileCodeRepo.ListPickups(ctx, f.Code, pickupDetailLimit); perr == nil && len(rows) > 0 {
			recs := make([]*sharemodel.PickupRecord, 0, len(rows))
			for _, r := range rows {
				rec := &sharemodel.PickupRecord{
					PickedAt: r.ChargedAt.UTC().Format(time.RFC3339Nano),
					IP:       r.IP,
				}
				if r.Channel != "" {
					ch := r.Channel
					rec.Channel = &ch
				}
				recs = append(recs, rec)
			}
			item.Pickups = recs
		}
		items = append(items, item)
	}
	return items, total, nil
}

// BatchDeleteUserShares 批量软删除
func (s *Service) BatchDeleteUserShares(ctx context.Context, userID uint, codes []string) (int, error) {
	n, err := s.fileCodeRepo.BatchSoftDeleteByCodes(ctx, userID, codes)
	if err == nil && n > 0 {
		// 有实际删除才撤销联邦公告；批量接口不回报实际命中的 code 子集，
		// 误撤未删码由心跳循环按 entries 重新补公告自愈（实现方约定）
		for _, code := range codes {
			s.federationDeleted(code)
		}
	}
	return n, err
}

// BatchExtendUserShares 批量延期
func (s *Service) BatchExtendUserShares(ctx context.Context, userID uint, codes []string, newExpireAt *time.Time) (int, error) {
	n, err := s.fileCodeRepo.BatchExtendByCodes(ctx, userID, codes, newExpireAt)
	if err == nil && n > 0 {
		// 联邦重公告(延期改变公告的 expires_at;误推未延期码由心跳按 entries 重新对齐)
		for _, code := range codes {
			s.federationCreated(code, newExpireAt)
		}
	}
	return n, err
}

// RestoreUserShare 恢复软删除的分享。0 行恢复（硬删/不存在/本就活跃）
// 显式报错——此前 0 行也返回 nil，硬删后 restore 表现为 200 静默 no-op
// （2026-10-08 修复；与 v0.11.1 hard-delete 同款收口）。
func (s *Service) RestoreUserShare(ctx context.Context, userID uint, code string) error {
	n, err := s.fileCodeRepo.RestoreByCode(ctx, userID, code)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotInRecycleBin
	}
	// 联邦重新公告(恢复=此前撤销的口令路由重新上线;查不到记录则跳过)
	if s.federation != nil {
		if file, err := s.fileCodeRepo.GetByCode(ctx, code); err == nil {
			s.federationCreated(file.Code, file.ExpiredAt)
		}
	}
	return nil
}

// ErrNotInRecycleBin 分享不在回收站（未软删/已硬删/不存在），restore 与
// hard-delete 共用。0 行变更曾双双表现为 200 静默 no-op——hard 侧 v0.11.1
// 修复、restore 侧 2026-10-08 修复：均改为调用方显式 400。
var ErrNotInRecycleBin = errors.New("分享不在回收站中（不存在或未处于软删除状态）")

// HardDeleteUserShare 永久删除软删除的分享
func (s *Service) HardDeleteUserShare(ctx context.Context, userID uint, code string) error {
	n, err := s.fileCodeRepo.HardDeleteByCode(ctx, userID, code)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotInRecycleBin
	}
	s.federationDeleted(code)
	return nil
}

// RecordViewer 记录取件人 IP / 时间（用于 owner 查看取件历史）
// 取件流程中调用。返回 fileCode 供后续通知使用
func (s *Service) RecordViewer(ctx context.Context, code, viewerIP string) (*model.FileCode, error) {
	if err := s.fileCodeRepo.UpdateViewer(ctx, code, viewerIP); err != nil {
		return nil, err
	}
	return s.fileCodeRepo.GetByCode(ctx, code)
}

// toUserShareListItem model → 列表项
func toUserShareItemData(f *model.FileCode) *sharemodel.UserShareItemData {
	isTextShare := f.IsTextShare()
	item := &sharemodel.UserShareItemData{
		ID:           int64(f.ID),
		Code:         f.Code,
		Prefix:       f.Prefix,
		Suffix:       f.Suffix,
		FilePath:     f.FilePath,
		Size:         f.Size,
		Text:         f.Text,
		ExpiredAt:    formatTimePtr(f.ExpiredAt),
		ExpiredCount: int32(f.ExpiredCount),
		UsedCount:    int32(f.UsedCount),
		RequireAuth:  f.RequireAuth,
		UploadType:   f.UploadType,
		CreatedAt:    f.CreatedAt.Format(time.RFC3339Nano),
		UpdatedAt:    f.UpdatedAt.Format(time.RFC3339Nano),
		DeletedAt:    formatTimePtr(deletedAtToPtr(f.DeletedAt)),
		ViewerIP:     f.ViewerIP,
		ViewerAt:     formatTimePtr(f.ViewerAt),
		ViewerCount:  int32(f.ViewerCount),
		// 「不可取件」= 时间过期 或 次数耗尽（2026-10-24 起两者分开判定，
		// 但列表侧要合并展示 ⇒ owner 看到的仍是同一个「已过期」）。
		IsExpired: f.IsExpired() || f.IsCountExhausted(),
		Status:    f.Status,
		// 文件分享的 Text 存原始文件名（非空），须用 IsTextShare 判定（Text 非空且无文件路径）
		IsTextShare: isTextShare,
	}
	if f.PickupCode != nil {
		item.PickupCode = f.PickupCode
	}
	if f.Note != "" {
		item.Note = &f.Note
	}
	// 文件名提取：文件分享依次取原始文件名（Text）、UUID 名、路径尾段；
	// 纯文本分享的 Text 是内容，不留文件名
	fileName := ""
	if !isTextShare {
		fileName = f.Text
		if fileName == "" {
			fileName = f.UUIDFileName
		}
		if fileName == "" && f.FilePath != "" {
			parts := strings.Split(f.FilePath, "/")
			if len(parts) > 0 {
				fileName = parts[len(parts)-1]
			}
		}
	}
	item.FileName = fileName
	return item
}

// formatTimePtr *time.Time → RFC3339Nano 字符串（nil 透传；与 time.Time 原生
// JSON 序列化同格式，wire 兼容手写契约时代的历史输出）
func formatTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.RFC3339Nano)
	return &s
}
