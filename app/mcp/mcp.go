// Package mcp 实现 PigeonBox 的 Model Context Protocol (MCP) server。
//
// 传输：Streamable HTTP（POST 单端点，JSON-RPC 2.0；与 MCP 规范对齐，
// Claude Desktop / 任意标准 MCP 客户端可直接接入）。legacy 文档中的裸 TCP
// 方案不符合 MCP 规范，本实现为标准重做。
//
// 认证与作用域（2026-10-12 访客快速分享，对标上游 v2.8 MCP quick share 并超出）：
// 默认路由挂 AdminMiddleware（管理员 JWT）全量工具；PB_MCP_ALLOW_GUEST=true 时
// 改挂可选认证，按调用者身份分三级作用域——游客（未登录）仅 share_text/share_file
// 快速分享（与网页匿名寄件同链路、同限流、同审核/白名单），登录用户 +=
// create_request_code/preview_share，管理员 = 全量。
//
// 工具集（对齐 legacy docs/mcp-server-guide.md + 2026-10-06 上传/下载/联邦扩展
// + 2026-10-12 寄件码/预览工具）：
//
//	share_text / share_file / create_request_code / preview_share /
//	get_share / get_share_content / download_share_file /
//	list_shares / delete_share / get_system_status / get_storage_info /
//	list_users / cleanup_expired / federation_status / federation_resolve
package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pigeonbox/core/pkg/baseurl"
	"github.com/pigeonbox/core/pkg/utils"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm"
)

// 协议常量
const (
	protocolVersion = "2025-03-26"
	serverName      = "pigeonbox"
)

// JSON-RPC 错误码（-32000 段为 server 自定义）
const (
	errParse     = -32700
	errInvalidRe = -32600
	errMethodNF  = -32601
	errInvalidPa = -32602
	errInternal  = -32603
	errToolFail  = -32000
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

// rpcRequest JSON-RPC 2.0 请求
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcResponse JSON-RPC 2.0 响应
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError JSON-RPC 2.0 错误对象
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
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

// P2pStatus 直传状态（P2b 设备直传工具展示用）。
type P2pStatus struct {
	Available     bool
	Enabled       bool
	DeployAllowed bool
	BinaryPath    string
	RegistryURL   string
	RecvDir       string
	InboxUsed     int64
	InboxQuota    int64
}

// P2pTaskInfo 直传任务摘要（P2b）。
type P2pTaskInfo struct {
	ID         string
	Kind       string // send | recv
	State      string
	Code       string
	BytesDone  int64
	Error      string
	CreatedAt  string
	FinishedAt string
}

// P2pAPI 设备直传服务窄接口（supervisor app/p2px 经适配器桥接；
// P2b 2026-10-14，spec §9 薄包装——admin 作用域，与 Web 操作页同服务层）。
// 2026-10-15 扩名录与邀请：P2pPeers/P2pInvites/P2pAccept + P2pSend 邀请参数。
type P2pAPI interface {
	P2pStatus(ctx context.Context) (P2pStatus, error)
	P2pTasks(ctx context.Context) ([]P2pTaskInfo, error)
	// P2pSend 发起发送；inviteTo 非空=向名录对端投递密封邀请。
	P2pSend(ctx context.Context, paths []string, inviteTo, inviteNote string) (P2pTaskInfo, error)
	P2pRecv(ctx context.Context, code string) (P2pTaskInfo, error)
	// P2pPeers 在线名录（opt-in 可发现节点）。
	P2pPeers(ctx context.Context) ([]P2pPeerInfo, error)
	// P2pInvites 待收邀请箱快照。
	P2pInvites(ctx context.Context) ([]P2pInviteInfo, error)
	// P2pAccept 认领邀请并立即发起接收。
	P2pAccept(ctx context.Context, inviteID string) (P2pTaskInfo, error)
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
	// CreateTextShare 创建文本分享，返回 (取件码, 完整链接)。userID=nil 匿名归因。
	CreateTextShare(ctx context.Context, text string, expireValue int, expireStyle string,
		requireAuth bool, passwordHash string, userID *uint, ownerIP, customCode string) (code, fullURL string, err error)
	// ShareBytes 内存内容建文件分享（MCP 通道：落存储 + 配额/审核/白名单链路）。
	ShareBytes(ctx context.Context, opts FileShareOpts) (code, fullURL string, err error)
	// ShareFiles 取分享的子文件清单。
	ShareFiles(ctx context.Context, code string) ([]ShareFileInfo, error)
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
	p2pSvc         P2pAPI // 读写走 p2pMu（装配期注入；P2b 直传工具）
	p2pMu          sync.RWMutex
	requestSvc     RequestAPI
	previewSvc     PreviewAPI
	version        string
	maxFileSizeCfg int64
	baseURL        string // 对外 base（server.base_url；空=按请求来源推断，见 pkg/baseurl）

	// fileCodeRepo 直查展示用（get_share/list_shares；构造注入，去全局化终态）
	fileCodeRepo *dao.FileCodeRepository
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

// SetP2pService 注入设备直传适配器（P2b；未启用传 nil）。
func (s *Service) SetP2pService(svc P2pAPI) {
	s.p2pMu.Lock()
	defer s.p2pMu.Unlock()
	s.p2pSvc = svc
}

// currentP2p 读当前直传适配器（并发安全快照）。
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

// Handle 处理一次 JSON-RPC POST。
// 返回 (httpStatus, responseBody)；通知类请求返回 (202, "")。
// caller 为 transport 层提取的调用者身份（游客=零值），驱动工具作用域过滤。
func (s *Service) Handle(ctx context.Context, body []byte, caller Caller) (int, []byte) {
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return writeRPCError(nil, errParse, "parse error: invalid JSON")
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		return writeRPCError(req.ID, errInvalidRe, "invalid request: jsonrpc must be \"2.0\" with a method")
	}

	// 通知（无 id）：不返回响应体（MCP 规范：202 Accepted）
	if len(req.ID) == 0 || string(req.ID) == "null" {
		return 202, nil
	}

	switch req.Method {
	case "initialize":
		return s.handleInitialize(req.ID)
	case "ping":
		return writeRPCResult(req.ID, map[string]any{})
	case "tools/list":
		return s.handleToolsList(req.ID, caller)
	case "tools/call":
		return s.handleToolsCall(ctx, req.ID, req.Params, caller)
	default:
		return writeRPCError(req.ID, errMethodNF, fmt.Sprintf("method not found: %s", req.Method))
	}
}

// handleInitialize MCP 握手
func (s *Service) handleInitialize(id json.RawMessage) (int, []byte) {
	return writeRPCResult(id, map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{
			"name":    serverName,
			"version": s.version,
		},
	})
}

// ============ 工具定义 ============

type toolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
	MinScope    int    `json:"-"` // 最低作用域（scopeGuest/scopeUser/scopeAdmin）
}

func objSchema(props map[string]any, required []string) map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func arrayProp(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

func (s *Service) toolDefs() []toolDef {
	return []toolDef{
		{Name: "share_text", MinScope: scopeGuest, Description: "创建文本分享，返回取件码、分享链接、Markdown 链接", InputSchema: objSchema(map[string]any{
			"text":         strProp("要分享的文本内容（上限 222KB）"),
			"expire_value": intProp("过期数值，默认 1"),
			"expire_style": strProp("过期样式：minute/hour/day/week/month/year/forever，默认 day"),
			"password":     strProp("可选取件密码（提供即开启密码保护）"),
			"custom_code":  strProp("可选自定义取件码（3-32 位字母/数字/-/_，冲突报错；仅登录用户）"),
		}, []string{"text"})},
		{Name: "share_file", MinScope: scopeGuest, Description: "上传文件并创建文件分享（base64 内容，走配额/审核/扩展名白名单链路），返回取件码、分享链接、Markdown 链接与命令行取件命令", InputSchema: objSchema(map[string]any{
			"file_name":      strProp("文件名（须通过扩展名白名单）"),
			"content_base64": strProp("文件内容的 base64 编码"),
			"expire_value":   intProp("过期数值，默认 1"),
			"expire_style":   strProp("过期样式：minute/hour/day/week/month/year/forever，默认 day"),
			"password":       strProp("可选取件密码（提供即开启密码保护）"),
			"custom_code":    strProp("可选自定义取件码（3-32 位字母/数字/-/_，冲突报错；仅登录用户）"),
		}, []string{"file_name", "content_base64"})},
		{Name: "create_request_code", MinScope: scopeUser, Description: "创建寄件码（反向收件投递链接）：发给对方让其上传文件，落成你名义的分享", InputSchema: objSchema(map[string]any{
			"title":        strProp("投递链接标题（显示给访客）"),
			"max_files":    intProp("最大文件数（0=不限）"),
			"max_bytes":    intProp("总大小上限字节（0=不限）"),
			"expire_value": intProp("过期数值，默认 7"),
			"expire_style": strProp("过期样式：minute/hour/day/week/month/year，默认 day"),
		}, nil)},
		{Name: "preview_share", MinScope: scopeUser, Description: "取分享内容预览（文本文件返回摘要文本；图片/视频/文档返回元数据；受取件闸门与密码保护约束）", InputSchema: objSchema(map[string]any{
			"code":     strProp("8 位分享码"),
			"password": strProp("密码保护分享的取件密码"),
		}, []string{"code"})},
		{Name: "get_share", MinScope: scopeAdmin, Description: "按取件码查询分享信息（不消耗取件次数）", InputSchema: objSchema(map[string]any{
			"code": strProp("8 位分享码"),
		}, []string{"code"})},
		{Name: "get_share_content", MinScope: scopeAdmin, Description: "读取分享内容（文本返回正文；文件返回元数据与文件清单；不消耗取件次数）", InputSchema: objSchema(map[string]any{
			"code": strProp("8 位分享码"),
		}, []string{"code"})},
		{Name: "download_share_file", MinScope: scopeAdmin, Description: "下载文件分享内容（base64 回传；仅单文件分享，已过期拒绝；不消耗取件次数）", InputSchema: objSchema(map[string]any{
			"code": strProp("8 位分享码"),
		}, []string{"code"})},
		{Name: "list_shares", MinScope: scopeAdmin, Description: "分页列出全站分享记录（可按取件码/文件名搜索）", InputSchema: objSchema(map[string]any{
			"page":      intProp("页码，默认 1"),
			"page_size": intProp("每页条数，默认 20，上限 100"),
			"keyword":   strProp("搜索关键字（取件码）"),
		}, nil)},
		{Name: "delete_share", MinScope: scopeAdmin, Description: "删除指定分享（DB 记录 + 物理文件，写审计日志）", InputSchema: objSchema(map[string]any{
			"code": strProp("8 位分享码"),
		}, []string{"code"})},
		{Name: "get_system_status", MinScope: scopeAdmin, Description: "系统运行状态（文件/用户/上传统计）", InputSchema: objSchema(nil, nil)},
		{Name: "get_storage_info", MinScope: scopeAdmin, Description: "存储使用状态（类型/容量/文件数）", InputSchema: objSchema(nil, nil)},
		{Name: "list_users", MinScope: scopeAdmin, Description: "分页列出用户", InputSchema: objSchema(map[string]any{
			"page":      intProp("页码，默认 1"),
			"page_size": intProp("每页条数，默认 20"),
		}, nil)},
		{Name: "cleanup_expired", MinScope: scopeAdmin, Description: "清理全部过期分享（DB + 物理文件）", InputSchema: objSchema(nil, nil)},
		{Name: "federation_status", MinScope: scopeAdmin, Description: "P2P 联邦状态（是否启用/节点 ID/注册中心/最近心跳）", InputSchema: objSchema(nil, nil)},
		{Name: "federation_resolve", MinScope: scopeAdmin, Description: "查询口令在联邦内的源节点（未接入联邦则不可达）", InputSchema: objSchema(map[string]any{
			"code": strProp("8 位分享码"),
		}, []string{"code"})},
		// P2b 设备直传四工具（admin 专属；与 Web「设备直传」操作页同服务层）
		{Name: "p2p_status", MinScope: scopeAdmin, Description: "P2P 设备直传状态（启用/p2pc 二进制/注册中心/收件区用量）", InputSchema: objSchema(nil, nil)},
		{Name: "p2p_tasks", MinScope: scopeAdmin, Description: "列出直传任务（口令/方向/状态/字节/错误）", InputSchema: objSchema(nil, nil)},
		{Name: "p2p_send", MinScope: scopeAdmin, Description: "发起设备直传发送：服务器白名单内路径→生成口令，对端凭码接收（口令窗口约 10 分钟）；可选 invite_to 向在线名录对端投递密封邀请（对端一键接收，NAT 后可达）", InputSchema: objSchema(map[string]any{
			"paths":       arrayProp("要发送的服务器本地绝对路径（须在 send_roots 白名单内）"),
			"invite_to":   strProp("选填：名录对端 node_id（64 位 hex，经 p2p_peers 查询）——发送后向其信箱投递密封邀请"),
			"invite_note": strProp("选填：邀请附言（≤200 字符）"),
		}, []string{"paths"})},
		{Name: "p2p_recv", MinScope: scopeAdmin, Description: "凭口令接收设备直传（文件落收件暂存区）", InputSchema: objSchema(map[string]any{
			"code": strProp("12 位直传口令，如 XXXX-XXXX-XXXX"),
		}, []string{"code"})},
		{Name: "p2p_peers", MinScope: scopeAdmin, Description: "在线名录：列出可发现的 P2P 对端（node_id/名称/对外地址/上线时刻；名录 opt-in，零敏感）", InputSchema: objSchema(nil, nil)},
		{Name: "p2p_invites", MinScope: scopeAdmin, Description: "列出待收邀请（其他实例经名录推送的密封邀请，TTL 10 分钟）", InputSchema: objSchema(nil, nil)},
		{Name: "p2p_accept", MinScope: scopeAdmin, Description: "认领待收邀请并立即发起接收（等价于 Web 待收面板的「接收」按钮）", InputSchema: objSchema(map[string]any{
			"invite_id": strProp("待收邀请 id（经 p2p_invites 查询）"),
		}, []string{"invite_id"})},
	}
}

// toolByName 按名查工具定义（作用域复核用）。
func (s *Service) toolByName(name string) (toolDef, bool) {
	for _, d := range s.toolDefs() {
		if d.Name == name {
			return d, true
		}
	}
	return toolDef{}, false
}

func (s *Service) handleToolsList(id json.RawMessage, caller Caller) (int, []byte) {
	level := caller.scopeLevel()
	visible := make([]toolDef, 0, len(s.toolDefs()))
	for _, d := range s.toolDefs() {
		if d.MinScope <= level {
			visible = append(visible, d)
		}
	}
	return writeRPCResult(id, map[string]any{"tools": visible})
}

// ============ 工具执行 ============

// handleToolsCall 执行工具。工具内部失败返回 isError 内容块（MCP 惯例：
// 业务失败放进 result.isError，协议错误才用 JSON-RPC error）。
func (s *Service) handleToolsCall(ctx context.Context, id json.RawMessage, params json.RawMessage, caller Caller) (int, []byte) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return writeRPCError(id, errInvalidPa, "invalid params: "+err.Error())
	}

	text, isErr := s.execTool(ctx, p.Name, p.Arguments, caller)
	result := map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
	}
	if isErr {
		result["isError"] = true
	}
	return writeRPCResult(id, result)
}

// execTool 单工具执行，返回 (文本结果, 是否错误)
func (s *Service) execTool(ctx context.Context, name string, args json.RawMessage, caller Caller) (string, bool) {
	// 作用域复核（tools/list 过滤不是授权边界）
	def, known := s.toolByName(name)
	if !known {
		return "unknown tool: " + name, true
	}
	if def.MinScope > caller.scopeLevel() {
		return "该工具不可用（需要更高身份：" + scopeLabel(def.MinScope) + "）", true
	}

	var a map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return "invalid arguments: " + err.Error(), true
		}
	}
	argStr := func(k string) string { v, _ := a[k].(string); return v }
	argInt := func(k string, def int) int {
		if v, ok := a[k].(float64); ok && int(v) > 0 {
			return int(v)
		}
		return def
	}

	switch name {
	case "share_text":
		text := argStr("text")
		if strings.TrimSpace(text) == "" {
			return "text 不能为空", true
		}
		if s.shareSvc == nil {
			return "share service 未注入", true
		}
		style := argStr("expire_style")
		if style == "" {
			style = "day"
		}
		var passwordHash string
		if pw := argStr("password"); pw != "" {
			hash, err := utils.HashPassword(pw)
			if err != nil {
				return "密码处理失败", true
			}
			passwordHash = hash
		}
		code, fullURL, err := s.shareSvc.CreateTextShare(ctx, text,
			argInt("expire_value", 1), style, passwordHash != "", passwordHash,
			caller.userPtr(), mcpOwnerIP(caller), callerCustomCode(caller, argStr("custom_code")))
		if err != nil {
			return "创建分享失败: " + err.Error(), true
		}
		return shareCreatedText("", text, code, fullURL, passwordHash != "", false), false

	case "get_share":
		code := argStr("code")
		fc, err := s.fileCodeRepo.GetByCode(ctx, code)
		if err != nil {
			return "分享不存在或已过期", true
		}
		expire := "永久"
		if fc.ExpiredAt != nil {
			expire = fc.ExpiredAt.Format("2006-01-02 15:04:05")
		}
		// 文件分享的 Text 存原始文件名（非空），须用 IsTextShare 判定（Text 非空且无文件路径）
		kind := "文件"
		if fc.IsTextShare() {
			kind = "文本"
		}
		out := fmt.Sprintf("分享信息\n取件码: %s\n类型: %s\n内容/文件名: %s\n大小: %d 字节\n剩余次数: %d（-1 不限）\n已用次数: %d\n过期时间: %s\n创建时间: %s",
			fc.Code, kind, displayFileName(fc), fc.Size, fc.ExpiredCount, fc.UsedCount, expire, fc.CreatedAt.Format("2006-01-02 15:04:05"))
		// P0 多文件：附子文件清单（仅文件分享且存在子表行时）
		if s.shareSvc != nil && !fc.IsTextShare() {
			if items, lerr := s.shareSvc.ShareFiles(ctx, code); lerr == nil && len(items) > 1 {
				out += fmt.Sprintf("\n文件清单（%d 个）:", len(items))
				for _, it := range items {
					out += fmt.Sprintf("\n  - %s（%d 字节）", it.Name, it.Size)
				}
			}
		}
		return out, false

	case "list_shares":
		page := argInt("page", 1)
		pageSize := argInt("page_size", 20)
		if pageSize > 100 {
			pageSize = 100
		}
		files, total, err := s.fileCodeRepo.List(ctx, page, pageSize, argStr("keyword"))
		if err != nil {
			return "查询失败: " + err.Error(), true
		}
		var b strings.Builder
		fmt.Fprintf(&b, "共 %d 条分享（第 %d 页）\n", total, page)
		for _, f := range files {
			expire := "-"
			if f.ExpiredAt != nil {
				expire = f.ExpiredAt.Format("2006-01-02")
			}
			fmt.Fprintf(&b, "· %s | %s | %d 字节 | 用 %d 次 | 过期 %s\n",
				f.Code, displayFileName(f), f.Size, f.UsedCount, expire)
		}
		return b.String(), false

	case "share_file":
		if s.shareSvc == nil {
			return "share service 未注入", true
		}
		name := argStr("file_name")
		b64 := argStr("content_base64")
		if strings.TrimSpace(name) == "" || strings.TrimSpace(b64) == "" {
			return "file_name 与 content_base64 不能为空", true
		}
		content, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return "content_base64 不是合法 base64: " + err.Error(), true
		}
		if int64(len(content)) > s.mcpMaxFileSize() {
			return fmt.Sprintf("文件 %d 字节超过 MCP 上限（%d 字节，PB_MCP_MAX_FILE_SIZE 可调；超过请求体上限还需调大 upload.max_file_size）",
				len(content), s.mcpMaxFileSize()), true
		}
		style := argStr("expire_style")
		if style == "" {
			style = "day"
		}
		var passwordHash string
		if pw := argStr("password"); pw != "" {
			hash, herr := utils.HashPassword(pw)
			if herr != nil {
				return "密码处理失败", true
			}
			passwordHash = hash
		}
		code, fullURL, err := s.shareSvc.ShareBytes(ctx, FileShareOpts{
			FileName: name, Content: content,
			ExpireValue: argInt("expire_value", 1), ExpireStyle: style,
			PasswordHash: passwordHash, CustomCode: callerCustomCode(caller, argStr("custom_code")),
			UserID: caller.userPtr(), OwnerIP: mcpOwnerIP(caller),
		})
		if err != nil {
			return "创建文件分享失败: " + err.Error(), true
		}
		return shareCreatedText(name, "", code, fullURL, passwordHash != "", true), false

	case "create_request_code":
		if caller.UserID == 0 {
			return "寄件码创建需要登录身份", true
		}
		if s.requestSvc == nil {
			return "request service 未注入", true
		}
		token, expiredAt, err := s.requestSvc.Create(ctx, caller.UserID, RequestCreate{
			Title:       argStr("title"),
			MaxFiles:    argInt("max_files", 0),
			MaxBytes:    int64(argInt("max_bytes", 0)),
			ExpireValue: argInt("expire_value", 7),
			ExpireStyle: defaultStr(argStr("expire_style"), "day"),
		})
		if err != nil {
			return "创建寄件码失败: " + err.Error(), true
		}
		link := fmt.Sprintf("%s/#/request/%s", baseurl.Resolve(ctx, s.baseURL), token)
		return fmt.Sprintf("寄件码创建成功\n投递链接: %s\n过期时间: %s\n（把链接发给对方，其上传的文件将落成你名义的分享）",
			link, expiredAt.Format("2006-01-02 15:04:05")), false

	case "preview_share":
		if s.previewSvc == nil {
			return "preview service 未注入", true
		}
		res, err := s.previewSvc.Preview(ctx, PreviewReq{
			Code:     argStr("code"),
			UserID:   caller.userPtr(),
			ClientIP: caller.IP,
			Password: argStr("password"),
		})
		if err != nil {
			return "预览失败: " + err.Error(), true
		}
		var b strings.Builder
		fmt.Fprintf(&b, "预览 %s\n类型: %s\nMIME: %s\n大小: %d 字节", argStr("code"), res.PreviewType, res.MimeType, res.FileSize)
		if res.Width > 0 {
			fmt.Fprintf(&b, "\n尺寸: %dx%d", res.Width, res.Height)
		}
		if res.PageCount > 0 {
			fmt.Fprintf(&b, "\n页数: %d", res.PageCount)
		}
		if res.Duration > 0 {
			fmt.Fprintf(&b, "\n时长: %d 秒", res.Duration)
		}
		if res.TextContent != "" {
			content := res.TextContent
			const capRunes = 8000
			if r := []rune(content); len(r) > capRunes {
				content = string(r[:capRunes]) + "\n…（预览截断，全文见取件页或下载）"
			}
			fmt.Fprintf(&b, "\n----\n%s\n----", content)
		}
		return b.String(), false

	case "get_share_content":
		code := argStr("code")
		fc, err := s.fileCodeRepo.GetByCode(ctx, code)
		if err != nil {
			return "分享不存在或已过期", true
		}
		if fc.IsTextShare() {
			return fmt.Sprintf("文本分享 %s 内容（%d 字节）\n----\n%s\n----", code, len(fc.Text), fc.Text), false
		}
		if s.shareSvc == nil {
			return fmt.Sprintf("文件分享 %s：%s（%d 字节）", code, displayFileName(fc), fc.Size), false
		}
		items, lerr := s.shareSvc.ShareFiles(ctx, code)
		if lerr != nil || len(items) == 0 {
			return fmt.Sprintf("文件分享 %s：%s（%d 字节）", code, displayFileName(fc), fc.Size), false
		}
		var b strings.Builder
		fmt.Fprintf(&b, "文件分享 %s 共 %d 个文件（合计 %d 字节）：", code, len(items), fc.Size)
		for _, it := range items {
			fmt.Fprintf(&b, "\n  - %s（%d 字节）", it.Name, it.Size)
		}
		return b.String(), false

	case "download_share_file":
		code := argStr("code")
		fc, err := s.fileCodeRepo.GetByCode(ctx, code)
		if err != nil {
			return "分享不存在或已过期", true
		}
		if fc.IsExpired() {
			return "分享已过期（内容待清理任务释放，可先在管理端续期）", true
		}
		if fc.IsTextShare() {
			return "文本分享请用 get_share_content 读取正文", true
		}
		// 多文件分享主路径非用户预期内容，拒绝并引导（清单见 get_share_content）
		if s.shareSvc != nil {
			if items, lerr := s.shareSvc.ShareFiles(ctx, code); lerr == nil && len(items) > 1 {
				return fmt.Sprintf("多文件分享（%d 个）不支持整体下载，请用分享链接在 Web 端打包下载，或按文件分别处理", len(items)), true
			}
		}
		if s.storageSvc == nil {
			return "存储服务未注入", true
		}
		path := fc.GetFilePath()
		size, serr := s.storageSvc.GetFileSize(ctx, path)
		if serr != nil {
			return "读取文件大小失败: " + serr.Error(), true
		}
		if size > s.mcpMaxFileSize() {
			return fmt.Sprintf("文件 %d 字节超过 MCP 下载上限（%d 字节，PB_MCP_MAX_FILE_SIZE 可调）", size, s.mcpMaxFileSize()), true
		}
		data, derr := s.storageSvc.GetFile(ctx, path)
		if derr != nil {
			return "读取文件失败: " + derr.Error(), true
		}
		return fmt.Sprintf("文件下载\n文件名: %s\n大小: %d 字节\nbase64:\n%s",
			displayFileName(fc), len(data), base64.StdEncoding.EncodeToString(data)), false

	case "federation_status":
		fed := s.currentFederation()
		if fed == nil {
			return "联邦未启用（federation.enabled=false 或初始化失败，本站为独立模式）", false
		}
		st := fed.Status()
		// 适配器在联邦未启用时返回 Enabled:false（非 nil 接口包 nil 实例）
		if !st.Enabled {
			return "联邦未启用（federation.enabled=false 或初始化失败，本站为独立模式）", false
		}
		health := "异常/尚未心跳"
		if st.Healthy {
			health = "正常"
		}
		return fmt.Sprintf("联邦状态\n启用: 是\n节点 ID: %s\n注册中心: %s\n最近心跳: %s",
			st.NodeID, strings.Join(st.Registries, ", "), health), false

	case "federation_resolve":
		fed := s.currentFederation()
		if fed == nil {
			return "联邦未启用", true
		}
		info, rerr := fed.Resolve(argStr("code"))
		if rerr != nil {
			return "联邦解析失败: " + rerr.Error(), true
		}
		if info == nil {
			return "口令未接入联邦或已失效（联邦内不可达）", false
		}
		return fmt.Sprintf("联邦路由\n源节点: %s（%s）\n地址: %s\n过期: %s\n大小: %d 字节",
			info.Name, info.NodeID, info.URL,
			info.ExpiresAt.Format("2006-01-02 15:04:05"), info.SizeHint), false

	// ---- P2b 设备直传四工具 ----
	case "p2p_status":
		p2p := s.currentP2p()
		if p2p == nil {
			return "设备直传未装配（构建缺 p2px 域）", false
		}
		st, serr := p2p.P2pStatus(ctx)
		if serr != nil {
			return "直传状态获取失败: " + serr.Error(), true
		}
		if !st.Enabled {
			return "设备直传未启用（管理后台「P2P 与联邦」分区开启）", false
		}
		avail := "否"
		if st.Available {
			avail = "是(" + st.BinaryPath + ")"
		}
		return fmt.Sprintf("直传状态\n启用: 是\np2pc: %s\n注册中心: %s\n收件区: %s（%d/%d 字节）\nstandalone: %v",
			avail, st.RegistryURL, st.RecvDir, st.InboxUsed, st.InboxQuota, st.DeployAllowed), false

	case "p2p_tasks":
		p2p := s.currentP2p()
		if p2p == nil {
			return "设备直传未装配", false
		}
		tasks, terr := p2p.P2pTasks(ctx)
		if terr != nil {
			return "任务列表获取失败: " + terr.Error(), true
		}
		if len(tasks) == 0 {
			return "暂无直传任务", false
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "直传任务（%d）:\n", len(tasks))
		for _, tk := range tasks {
			id := tk.ID
			if len(id) > 8 {
				id = id[:8]
			}
			fmt.Fprintf(&sb, "[%s] %s %s code=%s bytes=%d", id, tk.Kind, tk.State, tk.Code, tk.BytesDone)
			if tk.Error != "" {
				fmt.Fprintf(&sb, " err=%s", tk.Error)
			}
			sb.WriteString("\n")
		}
		return sb.String(), false

	case "p2p_send":
		p2p := s.currentP2p()
		if p2p == nil {
			return "设备直传未装配", false
		}
		rawPaths, _ := a["paths"].([]any)
		paths := make([]string, 0, len(rawPaths))
		for _, p := range rawPaths {
			if ps, ok := p.(string); ok && strings.TrimSpace(ps) != "" {
				paths = append(paths, ps)
			}
		}
		if len(paths) == 0 {
			return "paths 不能为空（服务器本地绝对路径数组，须命中 send_roots 白名单）", true
		}
		inviteTo := strings.ToLower(strings.TrimSpace(argStr("invite_to")))
		inviteNote := argStr("invite_note")
		task, serr := p2p.P2pSend(ctx, paths, inviteTo, inviteNote)
		if serr != nil {
			return "发送任务创建失败: " + serr.Error(), true
		}
		invited := ""
		if inviteTo != "" {
			head := inviteTo
			if len(head) > 12 {
				head = head[:12]
			}
			invited = "\n密封邀请: 已投递至名录对端 " + head + "…（失败仅告警不影响发送，详见任务日志）"
		}
		if task.Code == "" {
			return fmt.Sprintf("发送任务已创建 id=%s（口令生成中，稍后 p2p_tasks 查看；口令窗口约 10 分钟）%s", task.ID, invited), false
		}
		return fmt.Sprintf("发送任务已创建 id=%s\n口令: %s（对端凭码接收；口令窗口约 10 分钟）%s", task.ID, task.Code, invited), false

	case "p2p_recv":
		p2p := s.currentP2p()
		if p2p == nil {
			return "设备直传未装配", false
		}
		code := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(argStr("code")), "-", ""))
		if code == "" {
			return "code 不能为空（12 位直传口令）", true
		}
		if len(code) == 12 {
			code = code[:4] + "-" + code[4:8] + "-" + code[8:]
		}
		task, rerr2 := p2p.P2pRecv(ctx, code)
		if rerr2 != nil {
			return "接收任务创建失败: " + rerr2.Error(), true
		}
		return fmt.Sprintf("接收任务已发起 id=%s code=%s（文件落收件暂存区）", task.ID, code), false

	case "p2p_peers":
		p2p := s.currentP2p()
		if p2p == nil {
			return "设备直传未装配", false
		}
		peers, perr := p2p.P2pPeers(ctx)
		if perr != nil {
			return "名录查询失败: " + perr.Error(), true
		}
		if len(peers) == 0 {
			return "名录暂无在线对端（对端须开启「可被发现」并保持直传启用）", false
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("在线名录 %d 个对端:\n", len(peers)))
		for _, p := range peers {
			sb.WriteString(fmt.Sprintf("- %s\n  node_id: %s\n  对外地址: %s\n", p.Name, p.NodeID, func() string {
				if p.URL == "" {
					return "（未公布，NAT 后形态）"
				}
				return p.URL
			}()))
		}
		sb.WriteString("经 p2p_send 的 invite_to 向其对端投递密封邀请。")
		return sb.String(), false

	case "p2p_invites":
		p2p := s.currentP2p()
		if p2p == nil {
			return "设备直传未装配", false
		}
		invites, ierr := p2p.P2pInvites(ctx)
		if ierr != nil {
			return "待收邀请查询失败: " + ierr.Error(), true
		}
		if len(invites) == 0 {
			return "暂无待收邀请", false
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("待收邀请 %d 条:\n", len(invites)))
		for _, it := range invites {
			sb.WriteString(fmt.Sprintf("- id=%s 口令=%s 来自=%s", it.ID, it.Code, it.FromName))
			if it.Note != "" {
				sb.WriteString(" 附言=" + it.Note)
			}
			sb.WriteString(fmt.Sprintf("（%d 分钟后过期；p2p_accept 认领）\n", it.ExpiresIn/60))
		}
		return sb.String(), false

	case "p2p_accept":
		p2p := s.currentP2p()
		if p2p == nil {
			return "设备直传未装配", false
		}
		id := strings.TrimSpace(argStr("invite_id"))
		if id == "" {
			return "invite_id 不能为空（经 p2p_invites 查询）", true
		}
		task, aerr := p2p.P2pAccept(ctx, id)
		if aerr != nil {
			return "认领失败: " + aerr.Error(), true
		}
		return fmt.Sprintf("已认领邀请，接收任务已发起 id=%s code=%s（完成后 p2p_tasks 查看进度）", task.ID, task.Code), false

	case "delete_share":
		code := argStr("code")
		fc, err := s.fileCodeRepo.GetByCode(ctx, code)
		if err != nil {
			return "分享不存在: " + code, true
		}
		if msg, missing := s.adminReady(); missing {
			return msg, true
		}
		if err := s.adminSvc.DeleteShareByID(ctx, fc.ID); err != nil {
			return "删除失败: " + err.Error(), true
		}
		return fmt.Sprintf("已删除分享 %s（%s）", code, displayFileName(fc)), false

	case "get_system_status":
		if msg, missing := s.adminReady(); missing {
			return msg, true
		}
		stats, err := s.adminSvc.SystemStats(ctx)
		if err != nil {
			return "查询失败: " + err.Error(), true
		}
		return fmt.Sprintf("系统状态\n版本: %s\n文件总数: %d\n用户总数: %d\n总占用: %d 字节\n今日上传: %d\n过期文件: %d",
			s.version, stats.TotalFiles, stats.TotalUsers, stats.TotalSize, stats.TodayUploads, stats.ExpiredFiles), false

	case "get_storage_info":
		if msg, missing := s.adminReady(); missing {
			return msg, true
		}
		st, err := s.adminSvc.StorageStatus(ctx)
		if err != nil {
			return "查询失败: " + err.Error(), true
		}
		return fmt.Sprintf("存储状态\n类型: %s\n总空间: %d 字节\n已用: %d 字节（%.1f%%）\n文件数: %d",
			st.StorageType, st.TotalSpace, st.UsedSpace, st.UsagePercent, st.FileCount), false

	case "list_users":
		if msg, missing := s.adminReady(); missing {
			return msg, true
		}
		users, total, err := s.adminSvc.Users(ctx, argInt("page", 1), argInt("page_size", 20))
		if err != nil {
			return "查询失败: " + err.Error(), true
		}
		var b strings.Builder
		fmt.Fprintf(&b, "共 %d 个用户\n", total)
		for _, u := range users {
			fmt.Fprintf(&b, "· #%d %s（%s）状态 %s\n", u.ID, u.Username, u.Email, u.Status)
		}
		return b.String(), false

	case "cleanup_expired":
		if msg, missing := s.adminReady(); missing {
			return msg, true
		}
		n, freed, err := s.adminSvc.CleanExpired(ctx)
		if err != nil {
			return "清理失败: " + err.Error(), true
		}
		return fmt.Sprintf("清理完成：%d 个过期分享，释放 %d 字节", n, freed), false

	default:
		return "unknown tool: " + name, true
	}
}

// displayFileName 分享展示名（文本取前 20 字符，文件取原始名/回退 UUID 名）
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

// defaultStr 空串回退默认值。
func defaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
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

// shareCreatedText 分享创建成功输出：取件码 + 链接 + Markdown 链接（AI 直接引用）
// + 命令行取件命令（无密码文件分享；密码分享须经取件页验密，直链闸门会拒绝）。
// name 为文件名（文件分享）；text 为文本内容（文本分享取前 20 字符做展示名）。
func shareCreatedText(name, text, code, fullURL string, hasPassword, isFile bool) string {
	display := name
	if !isFile {
		display = displayFileName(&model.FileCode{Text: text})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "分享创建成功\n取件码: %s\n分享链接: %s", code, fullURL)
	if display != "" {
		fmt.Fprintf(&b, "\nMarkdown: [%s](%s)", escapeMD(display), fullURL)
	}
	if hasPassword {
		b.WriteString("\n已开启密码保护：命令行取件需先经取件页验密")
	} else if isFile {
		fmt.Fprintf(&b, "\n命令行取件: wget --content-disposition \"%s\"", anonymousDownloadURL(fullURL, code))
	}
	return b.String()
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

// ============ JSON-RPC 输出辅助 ============

func writeRPCResult(id json.RawMessage, result any) (int, []byte) {
	raw, err := json.Marshal(result)
	if err != nil {
		return writeRPCError(id, errInternal, "marshal result failed")
	}
	out, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: id, Result: raw})
	return 200, out
}

func writeRPCError(id json.RawMessage, code int, msg string) (int, []byte) {
	if id == nil {
		id = json.RawMessage("null")
	}
	out, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
	return 200, out
}

// ToolNames 供测试/文档列出工具名
func (s *Service) ToolNames() []string {
	defs := s.toolDefs()
	names := make([]string, len(defs))
	for i, d := range defs {
		names[i] = d.Name
	}
	return names
}
