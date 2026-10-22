// Package mcp 实现 PigeonBox 的 Model Context Protocol (MCP) server。
//
// 传输：Streamable HTTP（POST 单端点 + JSON 响应体）。协议层由**官方 SDK**
// github.com/modelcontextprotocol/go-sdk 承载（2026-10-22 换掉自研 JSON-RPC
// 实现）：协议版本协商、JSON-RPC 帧、工具注册、错误语义全部交给 SDK，默认支持
// 全部已知版本（最新 2026-07-28；握手期旧版 2025-11-25 / 2025-06-18 /
// 2025-03-26 向下兼容，客户端报什么版本就协商什么版本）。legacy 文档中的裸 TCP
// 方案不符合 MCP 规范，本实现为标准重做。
//
// 认证与作用域（2026-10-12 访客快速分享，对标上游 v2.8 MCP quick share 并超出）：
// 默认路由挂 AdminMiddleware（管理员 JWT）全量工具；PB_MCP_ALLOW_GUEST=true 时
// 改挂可选认证，按调用者身份分三级作用域——游客（未登录）仅 share_text/share_file
// 快速分享（与网页匿名寄件同链路、同限流、同审核/白名单），登录用户 +=
// create_request_code/preview_share，管理员 = 全量。身份由 transport 层经
// WithCaller 注入 ctx（官方 SDK 把 http.Request 的 ctx 透传到工具 handler）。
//
// 工具集（对齐 legacy docs/mcp-server-guide.md + 2026-10-06 上传/下载/联邦扩展
// + 2026-10-12 寄件码/预览工具 + Wave B' 直传四工具退役）：
//
//	share_text / share_file / create_request_code / preview_share /
//	get_share / get_share_content / download_share_file /
//	list_shares / delete_share / get_system_status / get_storage_info /
//	list_users / cleanup_expired / federation_status / federation_resolve /
//	p2p_peers / p2p_invites / p2p_accept
package mcp

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm"
)

// ===== 调用者作用域（2026-10-12 访客快速分享）=====

// 作用域等级：工具按 MinScope 声明最低可见身份，tools/list 随调用者过滤，
// tools/call 复核（列表过滤不是授权边界）。
const (
	scopeGuest = iota // 未登录（仅 PB_MCP_ALLOW_GUEST=true 时路由可达）
	scopeUser         // 任意登录用户
	scopeAdmin        // 管理员
)

// Caller 调用者身份（transport 层从中间件注入的 ctx 提取；游客=零值）。
type Caller struct {
	Admin  bool
	UserID uint
	IP     string
}

// scopeLevel 调用者作用域等级。
func (c Caller) scopeLevel() int {
	switch {
	case c.Admin:
		return scopeAdmin
	case c.UserID != 0:
		return scopeUser
	default:
		return scopeGuest
	}
}

// userPtr 登录用户 ID 指针（游客=nil，供配额/归因/闸门消费）。
func (c Caller) userPtr() *uint {
	if c.UserID == 0 {
		return nil
	}
	u := c.UserID
	return &u
}

// ===== 消费侧窄接口（bootstrap 注入域适配器，本包不直接依赖 admin/share 包）=====

// SystemStats 系统状态工具所需统计事实。
type SystemStats struct {
	TotalFiles   int64
	TotalUsers   int64
	TotalSize    int64
	TodayUploads int64
	ExpiredFiles int64
}

// StorageStatusInfo 存储状态工具所需事实。
type StorageStatusInfo struct {
	StorageType  string
	TotalSpace   int64
	UsedSpace    int64
	UsagePercent float64
	FileCount    int64
}

// UserRow 用户列表工具所需最小身份字段。
type UserRow struct {
	ID       uint
	Username string
	Email    string
	Status   string
}

// ShareFileInfo 分享子文件清单项。
type ShareFileInfo struct {
	Name string
	Size int64
}

// FileShareOpts 文件分享创建参数（share_file 工具）。
type FileShareOpts struct {
	FileName     string
	Content      []byte
	ExpireValue  int
	ExpireStyle  string
	PasswordHash string
	CustomCode   string
	UserID       *uint // 归属（游客=nil 匿名归因；登录用户配额记账）
	OwnerIP      string
}

// RequestCreate 寄件码（反向收件投递链接）创建参数。
type RequestCreate struct {
	Title       string
	MaxFiles    int
	MaxBytes    int64
	ExpireValue int
	ExpireStyle string
}

// PreviewReq 预览请求（preview_share 工具；闸门/密码校验在 preview 域内执行）。
type PreviewReq struct {
	Code     string
	UserID   *uint
	ClientIP string
	Password string
}

// PreviewResult 预览结果（AI 友好的最小事实面）。
type PreviewResult struct {
	PreviewType string
	MimeType    string
	TextContent string
	FileSize    int64
	Width       int
	Height      int
	PageCount   int
	Duration    int
}

// FederationStatus 联邦状态工具所需事实。
type FederationStatus struct {
	Enabled    bool
	NodeID     string
	Healthy    bool
	Registries []string
}

// FederationResolve 联邦路由解析结果。
type FederationResolve struct {
	NodeID    string
	URL       string
	Name      string
	ExpiresAt time.Time
	SizeHint  int64
}

// StorageAPI 下载工具所需存储事实（bootstrap 注入全站唯一实例）。
type StorageAPI interface {
	GetFileSize(ctx context.Context, filePath string) (int64, error)
	GetFile(ctx context.Context, filePath string) ([]byte, error)
}

// FederationAPI P2P 联邦工具所需事实（未启用时 bootstrap 传 nil 实例适配器）。
type FederationAPI interface {
	Status() FederationStatus
	Resolve(code string) (*FederationResolve, error)
}

// P2pAPI 联邦面窄接口（在线名录 / 待收邀请 / 认领信封）。
//
// 2026-10-22 Wave B'：直传四工具（status/tasks/send/recv）随 Box 侧直传
// 编排层整体退役 —— 直传由独立应用 PigeonBeam 承担，Box 不再编排。
// p2p_accept 退化为**纯信封**（出箱 + 返回信封），与 HTTP 面同语义。
type P2pAPI interface {
	// P2pPeers 在线名录（opt-in 可发现节点）。
	P2pPeers(ctx context.Context) ([]P2pPeerInfo, error)
	// P2pInvites 待收邀请箱快照。
	P2pInvites(ctx context.Context) ([]P2pInviteInfo, error)
	// P2pAccept 认领邀请（纯信封：出箱 + 返回信封）。
	P2pAccept(ctx context.Context, inviteID string) (P2pInviteInfo, error)
}

// P2pPeerInfo 在线名录对端（零敏感五字段，p2pd 透传）。
type P2pPeerInfo struct {
	NodeID      string `json:"node_id"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	BoxPub      string `json:"box_pub"`
	OnlineSince int64  `json:"online_since"`
}

// P2pInviteInfo 待收邀请条目（P3 收件箱快照透传）。
type P2pInviteInfo struct {
	ID        string `json:"id"`
	Code      string `json:"code"`
	FromName  string `json:"from_name"`
	Note      string `json:"note"`
	ExpiresIn int64  `json:"expires_in"`
}

// AdminAPI 系统维护类工具所需的 admin 域能力（bootstrap 注入全站唯一实例适配器）。
type AdminAPI interface {
	DeleteShareByID(ctx context.Context, id uint) error
	SystemStats(ctx context.Context) (*SystemStats, error)
	StorageStatus(ctx context.Context) (*StorageStatusInfo, error)
	Users(ctx context.Context, page, pageSize int) ([]UserRow, int64, error)
	CleanExpired(ctx context.Context) (count, freed int64, err error)
}

// ShareAPI 分享创建/查询工具所需的 share 域能力。
type ShareAPI interface {
	// CreateTextShare 创建文本分享。userID=nil 匿名归因。
	CreateTextShare(ctx context.Context, text string, expireValue int, expireStyle string,
		requireAuth bool, passwordHash string, userID *uint, ownerIP, customCode string) (ShareCreated, error)
	// ShareBytes 内存内容建文件分享（MCP 通道：落存储 + 配额/审核/白名单链路）。
	ShareBytes(ctx context.Context, opts FileShareOpts) (ShareCreated, error)
	// ShareFiles 取分享的子文件清单。
	ShareFiles(ctx context.Context, code string) ([]ShareFileInfo, error)
}

// ShareCreated 分享创建结果：分享码（链接标识，8 位/自定义口令）+ 6 位取件码
// （E2E 密文等未铸造场景为空）+ 完整分享链接。两码并列输出，杜绝把分享码
// 误标成取件码（2026-10-16 双码收敛：主推取件码）。
type ShareCreated struct {
	Code         string
	PickupCode   string
	FullShareURL string
}

// RequestAPI 寄件码工具所需的 request 域能力（bootstrap 注入域适配器）。
type RequestAPI interface {
	Create(ctx context.Context, userID uint, req RequestCreate) (token string, expiredAt time.Time, err error)
}

// PreviewAPI 预览工具所需的 preview 域能力（bootstrap 注入域适配器）。
type PreviewAPI interface {
	Preview(ctx context.Context, req PreviewReq) (*PreviewResult, error)
}

// Service MCP server。adminSvc 提供统计/维护能力，shareSvc 提供分享创建，
// storageSvc 提供文件下载事实，fedSvc 提供联邦状态/路由（未启用时为 nil 适配器）；
// storage/DAO 直查用于 get_share/get_share_content/list_shares 展示。
type Service struct {
	adminSvc       AdminAPI
	shareSvc       ShareAPI
	storageSvc     StorageAPI
	fedSvc         FederationAPI // 读写走 fedMu——P1 后台热切换=运行期写
	fedMu          sync.RWMutex
	p2pSvc         P2pAPI // 读写走 p2pMu（装配期注入；联邦三工具）
	p2pMu          sync.RWMutex
	requestSvc     RequestAPI
	previewSvc     PreviewAPI
	version        string
	maxFileSizeCfg int64
	baseURL        string // 对外 base（server.base_url；空=按请求来源推断，见 pkg/baseurl）

	// fileCodeRepo 直查展示用（get_share/list_shares；构造注入，去全局化终态）
	fileCodeRepo *dao.FileCodeRepository

	// handlerOnce/handler：官方 SDK 的 Streamable HTTP 处理器（惰性构建，
	// 保证 SetMaxFileSize 等配置注入后再定请求体上限）。
	handlerOnce sync.Once
	handler     http.Handler
}

// defaultMaxFileSize MCP 单文件上传/下载默认上限（6MB：base64 膨胀 4/3 后约 8MB，
// 低于默认请求体上限 10MB；调大时若超过请求体上限须同步调大 upload.max_file_size）。
const defaultMaxFileSize int64 = 6 << 20

// NewService 创建 MCP service。依赖经 Set*Service 注入（bootstrap 装配；
// 未注入时对应工具返回明确错误而非静默降级）。
func NewService(gormDB *gorm.DB, version string) *Service {
	return &Service{version: version, fileCodeRepo: dao.NewFileCodeRepository(gormDB)}
}

// SetAdminService 注入 admin 域能力适配器（统计/用户/维护）
func (s *Service) SetAdminService(svc AdminAPI) { s.adminSvc = svc }

// SetShareService 注入 share 域能力适配器（创建分享）
func (s *Service) SetShareService(svc ShareAPI) { s.shareSvc = svc }

// SetStorageService 注入存储能力（download_share_file）
func (s *Service) SetStorageService(svc StorageAPI) { s.storageSvc = svc }

// SetFederationService 注入 P2P 联邦适配器（未启用传 nil 实例适配器。
// P1 后台热切换后为运行期写，与工具调用并发——须加锁）
func (s *Service) SetFederationService(svc FederationAPI) {
	s.fedMu.Lock()
	defer s.fedMu.Unlock()
	s.fedSvc = svc
}

// currentFederation 读当前联邦适配器（并发安全快照）
func (s *Service) currentFederation() FederationAPI {
	s.fedMu.RLock()
	defer s.fedMu.RUnlock()
	return s.fedSvc
}

// SetP2pService 注入联邦面适配器（在线名录/待收邀请/认领信封；未装配传 nil）。
func (s *Service) SetP2pService(svc P2pAPI) {
	s.p2pMu.Lock()
	defer s.p2pMu.Unlock()
	s.p2pSvc = svc
}

// currentP2p 读当前联邦面适配器（并发安全快照）。
func (s *Service) currentP2p() P2pAPI {
	s.p2pMu.RLock()
	defer s.p2pMu.RUnlock()
	return s.p2pSvc
}

// SetRequestService 注入 request 域能力适配器（create_request_code）
func (s *Service) SetRequestService(svc RequestAPI) { s.requestSvc = svc }

// SetPreviewService 注入 preview 域能力适配器（preview_share）
func (s *Service) SetPreviewService(svc PreviewAPI) { s.previewSvc = svc }

// SetBaseURL 注入对外 base（server.base_url；空=按请求来源动态推断）
func (s *Service) SetBaseURL(base string) { s.baseURL = base }

// SetMaxFileSize 配置 MCP 单文件上限（字节；非正值回退默认）
func (s *Service) SetMaxFileSize(n int64) { s.maxFileSizeCfg = n }

// mcpMaxFileSize 生效的单文件上限
func (s *Service) mcpMaxFileSize() int64 {
	if s.maxFileSizeCfg > 0 {
		return s.maxFileSizeCfg
	}
	return defaultMaxFileSize
}

// adminReady 管理类工具依赖检查（明确报错，避免 nil 指针）。
func (s *Service) adminReady() (string, bool) {
	if s.adminSvc == nil {
		return "管理服务未注入（SetAdminService）", true
	}
	return "", false
}

func displayFileName(fc *model.FileCode) string {
	if fc == nil {
		return ""
	}
	if fc.Text != "" {
		name := fc.Text
		runes := []rune(name)
		if len(runes) > 20 {
			name = string(runes[:20]) + "…"
		}
		return name
	}
	if fc.UUIDFileName != "" {
		return fc.UUIDFileName
	}
	return fc.FilePath
}

// ============ 作用域/结果格式辅助（2026-10-12 访客快速分享）============

// scopeLabel 作用域等级中文标签（拒绝提示用）。
func scopeLabel(level int) string {
	switch level {
	case scopeAdmin:
		return "管理员"
	case scopeUser:
		return "登录用户"
	default:
		return "游客"
	}
}


// mcpOwnerIP 归因 IP（游客/用户同源；transport 层已做可信代理解析）。
func mcpOwnerIP(c Caller) string {
	if c.IP == "" {
		return "mcp"
	}
	return c.IP
}

// callerCustomCode 自定义取件码仅登录用户可用（与网页 handler 把关同语义）。
func callerCustomCode(c Caller, code string) string {
	if c.UserID == 0 {
		return ""
	}
	return code
}


// resolveFileCode 把工具输入解析为分享记录：恰 6 位输入先按取件码查 DB 真相源
// （pickup_code 列，语义对齐匿名取件链路 lookupShareCode 的回退序），未命中再按
// 分享码/自定义口令直查 code 列。此前管理类工具只收分享码，而管理员手里常只有
// 取件码（用户报障口径），6 位码会直接「分享不存在」。
func (s *Service) resolveFileCode(ctx context.Context, input string) (*model.FileCode, error) {
	code := strings.TrimSpace(input)
	if len(code) == 6 {
		code = strings.ToUpper(code)
		if fc, err := s.fileCodeRepo.GetByPickupCode(ctx, code); err == nil && fc != nil {
			return fc, nil
		}
	}
	return s.fileCodeRepo.GetByCode(ctx, code)
}

// escapeMD Markdown 链接显示文本转义（[]() 两组括号）。
func escapeMD(s string) string {
	s = strings.ReplaceAll(s, "[", "\\[")
	return strings.ReplaceAll(s, "]", "\\]")
}

// anonymousDownloadURL 由分享链接派生直链下载地址（fullURL 恒为 {base}/share/{code}
// 构型，见 app/share；baseurl 空时 fullURL 为相对路径，TrimSuffix 依旧成立）。
func anonymousDownloadURL(fullURL, code string) string {
	base := strings.TrimSuffix(fullURL, "/share/"+code)
	return base + "/anonymous/download/" + code
}

// ToolNames 供测试/文档列出工具名
func (s *Service) ToolNames() []string {
	names := make([]string, len(toolSpecs))
	for i, spec := range toolSpecs {
		names[i] = spec.Name
	}
	return names
}
