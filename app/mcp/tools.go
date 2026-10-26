package mcp

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/pigeonbox/core/pkg/baseurl"
	"github.com/pigeonbox/core/pkg/utils"
	"github.com/pigeonbox/core/repo/db/model"
)

// ============ 工具规格（注册表真相源）============
//
// 2026-10-22 工具层现代化：入参/出参全部类型化，由官方 SDK 推断 JSON Schema
// （2020-12 方言）并做**严格入参校验**（不认识的字段、类型错、缺必填项一律
// 回工具执行错误 → 模型可自纠，MCP SEP-1303）；出参同时进 structuredContent
// 与 content 文本块（JSON 形态），agent 不必再从散文里正则刮"分享码: XXXX"。
//
// 工具语义（名称/入参/出参）以本文件为真相源。

// toolSpec 工具元信息：名称/描述/最低作用域 + 注册动作。
// 顺序即 tools/list 的返回顺序。
type toolSpec struct {
	Name     string
	Desc     string
	MinScope int
	// Out 工具的出参结构（零值实例即可）⇒ 供**客观**判定「本工具是否对外交付
	// 内容」：反射出参里的内容字段（text / content_base64 / …）。
	//
	// 为什么要客观判据（2026-10-25 审计 A2）：守卫此前**只信作者自标的**
	// `Gate.ReadsShare` ⇒ 新增一个交付内容的工具而忘写 Gate ⇒ 机械守卫**不会**
	// 变红（`preview_share` 就是这样漏出契约表的）⇒ 守卫的发现机制自身有洞。
	// ⇒ 客观判据（出参含内容字段）由**出参结构**推导，与作者标注不一致即红。
	Out any
	// Gate 取件闸门策略：声明本工具是否**读分享内容/元信息**（ReadsShare），
	// 以及次数耗尽时的判定（Exhausted）。
	//
	// 为什么要显式声明（2026-10-24）：`IsExpired()` 拆分前含次数语义 ⇒
	// 「已过期」的工具天然也拒「次数耗尽」；拆分后每个工具各自判过期，
	// **次数闸就漏了**（实测 MCP 的 get_share_content / download_share_file
	// 在额度用完后仍回传正文/文件内容 ⇒ count 样式分享的「最多取 N 次」
	// 承诺在管理面完全失效）。
	//
	// ⇒ 把「读不读分享内容」标进**注册表本身**（单一真相源），由取件闸门
	// 契约守卫（bootstrap/share_surface_contract_test.go）逐个核对：
	// 凡是 ReadsShare=true 的工具，都必须在契约表里声明耗尽判定并有行为测试覆盖。
	Gate     *ShareGate
	Register func(s *Service, srv *sdk.Server)
}

// ShareGate 单个工具的取件闸门策略。
type ShareGate struct {
	// ReadsShare 本工具是否对外交付分享内容或元信息（true ⇒ 必须守次数闸）。
	ReadsShare bool
	// Exhausted 次数耗尽时的判定（错误文案/业务码口径）；ReadsShare=false 时留空。
	Exhausted string
	// CoveredBy 覆盖该策略的行为测试函数名（守卫机械校验其真实存在）。
	CoveredBy string
}

// toolSpecs 全部工具（18 个）。
var toolSpecs = []toolSpec{
	{
		Name: "share_text", MinScope: scopeGuest,
		Out:  shareCreatedOut{},
		Desc: "创建文本分享，返回取件码、分享链接与 Markdown 链接",
		Register: func(s *Service, srv *sdk.Server) {
			sdk.AddTool(srv, &sdk.Tool{Name: "share_text", Description: "创建文本分享，返回取件码、分享链接与 Markdown 链接"}, toolGuard(scopeGuest, s.shareText))
		},
	},
	{
		Name: "share_file", MinScope: scopeGuest,
		Out:  shareCreatedOut{},
		Desc: "上传文件并创建文件分享（base64 内容，走配额/审核/扩展名白名单链路），返回取件码、分享链接、Markdown 链接与直链",
		Register: func(s *Service, srv *sdk.Server) {
			sdk.AddTool(srv, &sdk.Tool{Name: "share_file", Description: "上传文件并创建文件分享（base64 内容，走配额/审核/扩展名白名单链路），返回取件码、分享链接、Markdown 链接与直链"}, toolGuard(scopeGuest, s.shareFile))
		},
	},
	{
		Name: "create_request_code", MinScope: scopeUser,
		Out:  createRequestCodeOut{},
		Desc: "创建寄件码（反向收件投递链接）：发给对方让其上传文件，落成你名义的分享",
		Register: func(s *Service, srv *sdk.Server) {
			sdk.AddTool(srv, &sdk.Tool{Name: "create_request_code", Description: "创建寄件码（反向收件投递链接）：发给对方让其上传文件，落成你名义的分享"}, toolGuard(scopeUser, s.createRequestCode))
		},
	},
	{
		Name: "preview_share", MinScope: scopeUser,
		Out: previewShareOut{},
		// 交付内容（文本摘要）⇒ 必须守取件次数闸（客观判据：出参含 Text 字段）。
		// 此前**漏标** ⇒ 取件闸门契约守卫按「自标」发现面时它漏出契约表
		// （2026-10-25 审计 A2：守卫的发现机制自身有洞）。
		Gate: &ShareGate{ReadsShare: true, Exhausted: "20004", CoveredBy: "TestPreview_CountExhausted_ReturnsReachLimit"},
		Desc: "取分享内容预览（文本文件返回摘要文本；图片/视频/文档返回元数据；受取件闸门与密码保护约束）",
		Register: func(s *Service, srv *sdk.Server) {
			sdk.AddTool(srv, &sdk.Tool{Name: "preview_share", Description: "取分享内容预览（文本文件返回摘要文本；图片/视频/文档返回元数据；受取件闸门与密码保护约束）"}, toolGuard(scopeUser, s.previewShare))
		},
	},
	{
		Name: "get_share", MinScope: scopeAdmin,
		Out:  getShareOut{},
		Desc: "按分享码或 6 位取件码查询分享信息（不消耗取件次数）",
		// 管理面元信息面：与 /admin/files 同规格（管理员本就可见全站分享的
		// 文件名/大小），**豁免**取件次数闸（次数闸约束的是「对外分发给收件人」）。
		Gate: &ShareGate{ReadsShare: true, Exhausted: "豁免"},
		Register: func(s *Service, srv *sdk.Server) {
			sdk.AddTool(srv, &sdk.Tool{Name: "get_share", Description: "按分享码或 6 位取件码查询分享信息（不消耗取件次数）"}, toolGuard(scopeAdmin, s.getShare))
		},
	},
	{
		Name: "get_share_content", MinScope: scopeAdmin,
		Out:  getShareContentOut{},
		Desc: "读取分享内容（文本返回正文；文件返回元数据与文件清单；不消耗取件次数）",
		// 交付面：文本分享这里**回传正文** ⇒ 必须守次数闸（count 样式分享的
		// 「最多取 N 次」承诺不能在管理面被绕过）。
		Gate: &ShareGate{ReadsShare: true, Exhausted: "取件次数已用完", CoveredBy: "TestToolsCall_CountExhaustedShareDenied"},
		Register: func(s *Service, srv *sdk.Server) {
			sdk.AddTool(srv, &sdk.Tool{Name: "get_share_content", Description: "读取分享内容（文本返回正文；文件返回元数据与文件清单；不消耗取件次数）"}, toolGuard(scopeAdmin, s.getShareContent))
		},
	},
	{
		Name: "download_share_file", MinScope: scopeAdmin,
		Out:  downloadShareFileOut{},
		Desc: "下载文件分享内容（base64 回传；仅单文件分享，已过期拒绝；不消耗取件次数）",
		// 交付面：回传**文件内容** ⇒ 必须守次数闸（同上）。
		Gate: &ShareGate{ReadsShare: true, Exhausted: "取件次数已用完", CoveredBy: "TestToolsCall_CountExhaustedFileShareDenied"},
		Register: func(s *Service, srv *sdk.Server) {
			sdk.AddTool(srv, &sdk.Tool{Name: "download_share_file", Description: "下载文件分享内容（base64 回传；仅单文件分享，已过期拒绝；不消耗取件次数）"}, toolGuard(scopeAdmin, s.downloadShareFile))
		},
	},
	{
		Name: "list_shares", MinScope: scopeAdmin,
		Out:  listSharesOut{},
		Desc: "分页列出全站分享记录（可按分享码/取件码/文件名搜索）",
		Register: func(s *Service, srv *sdk.Server) {
			sdk.AddTool(srv, &sdk.Tool{Name: "list_shares", Description: "分页列出全站分享记录（可按分享码/取件码/文件名搜索）"}, toolGuard(scopeAdmin, s.listShares))
		},
	},
	{
		Name: "delete_share", MinScope: scopeAdmin,
		Out:  deleteShareOut{},
		Desc: "删除指定分享（DB 记录 + 物理文件，写审计日志）",
		Register: func(s *Service, srv *sdk.Server) {
			sdk.AddTool(srv, &sdk.Tool{Name: "delete_share", Description: "删除指定分享（DB 记录 + 物理文件，写审计日志）"}, toolGuard(scopeAdmin, s.deleteShare))
		},
	},
	{
		Name: "get_system_status", MinScope: scopeAdmin,
		Desc: "系统运行状态（文件/用户/上传统计）",
		Register: func(s *Service, srv *sdk.Server) {
			sdk.AddTool(srv, &sdk.Tool{Name: "get_system_status", Description: "系统运行状态（文件/用户/上传统计）"}, toolGuard(scopeAdmin, s.getSystemStatus))
		},
	},
	{
		Name: "get_storage_info", MinScope: scopeAdmin,
		Desc: "存储使用状态（类型/容量/文件数）",
		Register: func(s *Service, srv *sdk.Server) {
			sdk.AddTool(srv, &sdk.Tool{Name: "get_storage_info", Description: "存储使用状态（类型/容量/文件数）"}, toolGuard(scopeAdmin, s.getStorageInfo))
		},
	},
	{
		Name: "list_users", MinScope: scopeAdmin,
		Desc: "分页列出用户",
		Register: func(s *Service, srv *sdk.Server) {
			sdk.AddTool(srv, &sdk.Tool{Name: "list_users", Description: "分页列出用户"}, toolGuard(scopeAdmin, s.listUsers))
		},
	},
	{
		Name: "cleanup_expired", MinScope: scopeAdmin,
		Desc: "清理全部过期分享（DB + 物理文件）",
		Register: func(s *Service, srv *sdk.Server) {
			sdk.AddTool(srv, &sdk.Tool{Name: "cleanup_expired", Description: "清理全部过期分享（DB + 物理文件）"}, toolGuard(scopeAdmin, s.cleanupExpired))
		},
	},
	{
		Name: "federation_status", MinScope: scopeAdmin,
		Desc: "P2P 联邦状态（是否启用/节点 ID/注册中心/最近心跳）",
		Register: func(s *Service, srv *sdk.Server) {
			sdk.AddTool(srv, &sdk.Tool{Name: "federation_status", Description: "P2P 联邦状态（是否启用/节点 ID/注册中心/最近心跳）"}, toolGuard(scopeAdmin, s.federationStatus))
		},
	},
	{
		Name: "federation_resolve", MinScope: scopeAdmin,
		Desc: "查询口令在联邦内的源节点（未接入联邦则不可达）",
		Register: func(s *Service, srv *sdk.Server) {
			sdk.AddTool(srv, &sdk.Tool{Name: "federation_resolve", Description: "查询口令在联邦内的源节点（未接入联邦则不可达）"}, toolGuard(scopeAdmin, s.federationResolve))
		},
	},
	{
		Name: "p2p_peers", MinScope: scopeAdmin,
		Desc: "在线名录：列出可发现的 P2P 对端（node_id/名称/对外地址/上线时刻；名录 opt-in，零敏感）",
		Register: func(s *Service, srv *sdk.Server) {
			sdk.AddTool(srv, &sdk.Tool{Name: "p2p_peers", Description: "在线名录：列出可发现的 P2P 对端（node_id/名称/对外地址/上线时刻；名录 opt-in，零敏感）"}, toolGuard(scopeAdmin, s.p2pPeers))
		},
	},
	{
		Name: "p2p_invites", MinScope: scopeAdmin,
		Desc: "列出待收邀请（其他实例经名录推送的密封邀请，TTL 10 分钟）",
		Register: func(s *Service, srv *sdk.Server) {
			sdk.AddTool(srv, &sdk.Tool{Name: "p2p_invites", Description: "列出待收邀请（其他实例经名录推送的密封邀请，TTL 10 分钟）"}, toolGuard(scopeAdmin, s.p2pInvites))
		},
	},
	{
		Name: "p2p_accept", MinScope: scopeAdmin,
		Desc: "认领待收邀请（纯信封：出箱并返回信封；直传接收请用 PigeonBeam 客户端完成）",
		Register: func(s *Service, srv *sdk.Server) {
			sdk.AddTool(srv, &sdk.Tool{Name: "p2p_accept", Description: "认领待收邀请（纯信封：出箱并返回信封；直传接收请用 PigeonBeam 客户端完成）"}, toolGuard(scopeAdmin, s.p2pAccept))
		},
	},
}

// ============ 公共出参片段 ============

// shareFileItem 分享的子文件清单项。
type shareFileItem struct {
	Name      string `json:"name" jsonschema:"文件名"`
	SizeBytes int64  `json:"size_bytes" jsonschema:"文件大小（字节）"`
}

// shareCreatedOut 分享创建结果（文本/文件共用）。
type shareCreatedOut struct {
	Code        string `json:"code" jsonschema:"分享码（8 位，链接标识）"`
	PickupCode  string `json:"pickup_code,omitempty" jsonschema:"6 位取件码（主推入口；未铸造时为空）"`
	ShareURL    string `json:"share_url" jsonschema:"完整分享链接"`
	Markdown    string `json:"markdown,omitempty" jsonschema:"Markdown 链接（可直接引用到回复/文档）"`
	DownloadURL string `json:"download_url,omitempty" jsonschema:"匿名直链（仅无密码的文件分享可用）"`
	HasPassword bool   `json:"has_password" jsonschema:"是否开启密码保护（取件须先验密）"`
	FileName    string `json:"file_name,omitempty" jsonschema:"文件名（文件分享）"`
	SizeBytes   int64  `json:"size_bytes,omitempty" jsonschema:"文件大小（字节；文件分享）"`
}

// ============ 工具 1/18：share_text ============

type shareTextIn struct {
	Text        string `json:"text" jsonschema:"要分享的文本内容（上限 222KB）"`
	ExpireValue int    `json:"expire_value,omitempty" jsonschema:"过期数值，默认 1"`
	ExpireStyle string `json:"expire_style,omitempty" jsonschema:"过期样式：minute/hour/day/week/month/year/forever/count（count 按取件次数，对方最多取 N 次），默认 day"`
	Password    string `json:"password,omitempty" jsonschema:"可选取件密码（提供即开启密码保护）"`
	CustomCode  string `json:"custom_code,omitempty" jsonschema:"可选自定义取件码（3-32 位字母/数字/-/_，冲突报错；仅登录用户）"`
}

func (s *Service) shareText(ctx context.Context, in shareTextIn, caller Caller) (shareCreatedOut, error) {
	if strings.TrimSpace(in.Text) == "" {
		return shareCreatedOut{}, fmt.Errorf("text 不能为空")
	}
	if s.shareSvc == nil {
		return shareCreatedOut{}, fmt.Errorf("share service 未注入")
	}
	style, err := normalizeExpireStyle(in.ExpireStyle)
	if err != nil {
		return shareCreatedOut{}, err
	}
	passwordHash, err := hashPassword(in.Password)
	if err != nil {
		return shareCreatedOut{}, err
	}
	created, err := s.shareSvc.CreateTextShare(ctx, in.Text,
		expireValue(in.ExpireValue, 1), style, passwordHash != "", passwordHash,
		caller.userPtr(), mcpOwnerIP(caller), callerCustomCode(caller, in.CustomCode))
	if err != nil {
		return shareCreatedOut{}, fmt.Errorf("创建分享失败: %w", err)
	}
	return s.shareCreatedOut(created, displayFileName(&model.FileCode{Text: in.Text}), "", 0, passwordHash != "", false), nil
}

// ============ 工具 2/18：share_file ============

type shareFileIn struct {
	FileName      string `json:"file_name" jsonschema:"文件名（须通过扩展名白名单）"`
	ContentBase64 string `json:"content_base64" jsonschema:"文件内容的 base64 编码"`
	ExpireValue   int    `json:"expire_value,omitempty" jsonschema:"过期数值，默认 1"`
	ExpireStyle   string `json:"expire_style,omitempty" jsonschema:"过期样式：minute/hour/day/week/month/year/forever/count（count 按取件次数，对方最多取 N 次），默认 day"`
	Password      string `json:"password,omitempty" jsonschema:"可选取件密码（提供即开启密码保护）"`
	CustomCode    string `json:"custom_code,omitempty" jsonschema:"可选自定义取件码（3-32 位字母/数字/-/_，冲突报错；仅登录用户）"`
}

func (s *Service) shareFile(ctx context.Context, in shareFileIn, caller Caller) (shareCreatedOut, error) {
	if s.shareSvc == nil {
		return shareCreatedOut{}, fmt.Errorf("share service 未注入")
	}
	if strings.TrimSpace(in.FileName) == "" || strings.TrimSpace(in.ContentBase64) == "" {
		return shareCreatedOut{}, fmt.Errorf("file_name 与 content_base64 不能为空")
	}
	content, err := base64.StdEncoding.DecodeString(in.ContentBase64)
	if err != nil {
		return shareCreatedOut{}, fmt.Errorf("content_base64 不是合法 base64: %w", err)
	}
	if int64(len(content)) > s.mcpMaxFileSize() {
		return shareCreatedOut{}, fmt.Errorf("文件 %d 字节超过 MCP 上限（%d 字节，PB_MCP_MAX_FILE_SIZE 可调；超过请求体上限还需调大 upload.max_file_size）",
			len(content), s.mcpMaxFileSize())
	}
	style, err := normalizeExpireStyle(in.ExpireStyle)
	if err != nil {
		return shareCreatedOut{}, err
	}
	passwordHash, err := hashPassword(in.Password)
	if err != nil {
		return shareCreatedOut{}, err
	}
	created, err := s.shareSvc.ShareBytes(ctx, FileShareOpts{
		FileName: in.FileName, Content: content,
		ExpireValue: expireValue(in.ExpireValue, 1), ExpireStyle: style,
		PasswordHash: passwordHash, CustomCode: callerCustomCode(caller, in.CustomCode),
		UserID: caller.userPtr(), OwnerIP: mcpOwnerIP(caller),
	})
	if err != nil {
		return shareCreatedOut{}, fmt.Errorf("创建文件分享失败: %w", err)
	}
	return s.shareCreatedOut(created, in.FileName, in.FileName, int64(len(content)), passwordHash != "", true), nil
}

// ============ 工具 3/18：create_request_code ============

type createRequestCodeIn struct {
	Title       string `json:"title,omitempty" jsonschema:"投递链接标题（显示给访客）"`
	MaxFiles    int    `json:"max_files,omitempty" jsonschema:"最大文件数（0 表示不限）"`
	MaxBytes    int64  `json:"max_bytes,omitempty" jsonschema:"总大小上限字节（0 表示不限）"`
	ExpireValue int    `json:"expire_value,omitempty" jsonschema:"过期数值，默认 7"`
	ExpireStyle string `json:"expire_style,omitempty" jsonschema:"过期样式：minute/hour/day/week/month/year/forever/count（count 按取件次数，对方最多取 N 次），默认 day"`
}

type createRequestCodeOut struct {
	Token       string `json:"token" jsonschema:"寄件码令牌（投递链接路径段）"`
	DeliveryURL string `json:"delivery_url" jsonschema:"投递链接（发给对方，其上传的文件将落成你名义的分享）"`
	ExpiresAt   string `json:"expires_at" jsonschema:"过期时间（RFC3339）"`
}

func (s *Service) createRequestCode(ctx context.Context, in createRequestCodeIn, caller Caller) (createRequestCodeOut, error) {
	if caller.UserID == 0 {
		return createRequestCodeOut{}, fmt.Errorf("寄件码创建需要登录身份")
	}
	if s.requestSvc == nil {
		return createRequestCodeOut{}, fmt.Errorf("request service 未注入")
	}
	style, err := normalizeExpireStyle(in.ExpireStyle)
	if err != nil {
		return createRequestCodeOut{}, err
	}
	token, expiredAt, err := s.requestSvc.Create(ctx, caller.UserID, RequestCreate{
		Title:       in.Title,
		MaxFiles:    in.MaxFiles,
		MaxBytes:    in.MaxBytes,
		ExpireValue: expireValue(in.ExpireValue, 7),
		ExpireStyle: style,
	})
	if err != nil {
		return createRequestCodeOut{}, fmt.Errorf("创建寄件码失败: %w", err)
	}
	return createRequestCodeOut{
		Token:       token,
		DeliveryURL: fmt.Sprintf("%s/#/request/%s", baseurl.Resolve(ctx, s.baseURL), token),
		ExpiresAt:   expiredAt.Format(time.RFC3339),
	}, nil
}

// ============ 工具 4/18：preview_share ============

type previewShareIn struct {
	Code     string `json:"code" jsonschema:"分享码（8 位）、6 位取件码或自定义口令"`
	Password string `json:"password,omitempty" jsonschema:"密码保护分享的取件密码"`
}

type previewShareOut struct {
	Code            string `json:"code" jsonschema:"分享码（已按分享码归一）"`
	PreviewType     string `json:"preview_type" jsonschema:"预览类型（text/image/video/document 等）"`
	MimeType        string `json:"mime_type" jsonschema:"MIME 类型"`
	SizeBytes       int64  `json:"size_bytes" jsonschema:"内容大小（字节）"`
	Width           int    `json:"width,omitempty" jsonschema:"图片宽度（像素）"`
	Height          int    `json:"height,omitempty" jsonschema:"图片高度（像素）"`
	PageCount       int    `json:"page_count,omitempty" jsonschema:"文档页数"`
	DurationSeconds int    `json:"duration_seconds,omitempty" jsonschema:"媒体时长（秒）"`
	Text            string `json:"text,omitempty" jsonschema:"文本摘要（超出上限会截断）"`
	Truncated       bool   `json:"truncated" jsonschema:"文本是否被截断"`
}

func (s *Service) previewShare(ctx context.Context, in previewShareIn, caller Caller) (previewShareOut, error) {
	if s.previewSvc == nil {
		return previewShareOut{}, fmt.Errorf("preview service 未注入")
	}
	// 6 位取件码换算分享码为最佳努力：解析失败透传原码，由 preview 域
	// 自行报不存在（不存在性校验 preview 域本就有，此处只做码型翻译）。
	previewCode := in.Code
	if s.fileCodeRepo != nil {
		if fc, rerr := s.resolveFileCode(ctx, previewCode); rerr == nil {
			previewCode = fc.Code
		}
	}
	res, err := s.previewSvc.Preview(ctx, PreviewReq{
		Code:     previewCode,
		UserID:   caller.userPtr(),
		ClientIP: caller.IP,
		Password: in.Password,
	})
	if err != nil {
		return previewShareOut{}, fmt.Errorf("预览失败: %w", err)
	}
	text := res.TextContent
	truncated := false
	const capRunes = 8000
	if r := []rune(text); len(r) > capRunes {
		text = string(r[:capRunes]) + "\n…（预览截断，全文见取件页或下载）"
		truncated = true
	}
	return previewShareOut{
		Code:            previewCode,
		PreviewType:     res.PreviewType,
		MimeType:        res.MimeType,
		SizeBytes:       res.FileSize,
		Width:           res.Width,
		Height:          res.Height,
		PageCount:       res.PageCount,
		DurationSeconds: res.Duration,
		Text:            text,
		Truncated:       truncated,
	}, nil
}

// ============ 工具 5/18：get_share ============

type getShareIn struct {
	Code string `json:"code" jsonschema:"分享码（8 位）、6 位取件码或自定义口令"`
}

type getShareOut struct {
	Code           string          `json:"code" jsonschema:"分享码"`
	PickupCode     string          `json:"pickup_code,omitempty" jsonschema:"6 位取件码（未铸造时为空）"`
	Kind           string          `json:"kind" jsonschema:"分享类型：text 为文本，file 为文件"`
	Name           string          `json:"name" jsonschema:"展示名（文本取前 20 字符，文件取原始名）"`
	SizeBytes      int64           `json:"size_bytes" jsonschema:"内容大小（字节）"`
	RemainingCount int             `json:"remaining_count" jsonschema:"剩余取件次数（-1 表示不限，0 表示已耗尽）"`
	UsedCount      int             `json:"used_count" jsonschema:"已用取件次数"`
	ExpiresAt      string          `json:"expires_at,omitempty" jsonschema:"过期时间（RFC3339；永久分享为空）"`
	CreatedAt      string          `json:"created_at" jsonschema:"创建时间（RFC3339）"`
	Files          []shareFileItem `json:"files,omitempty" jsonschema:"子文件清单（多文件分享）"`
}

func (s *Service) getShare(ctx context.Context, in getShareIn, caller Caller) (getShareOut, error) {
	fc, err := s.resolveFileCode(ctx, in.Code)
	if err != nil {
		return getShareOut{}, fmt.Errorf("分享不存在或已过期")
	}
	out := getShareOut{
		Code:           fc.Code,
		PickupCode:     derefString(fc.PickupCode),
		Kind:           shareKind(fc),
		Name:           displayFileName(fc),
		SizeBytes:      fc.Size,
		RemainingCount: fc.ExpiredCount,
		UsedCount:      fc.UsedCount,
		CreatedAt:      fc.CreatedAt.Format(time.RFC3339),
	}
	if fc.ExpiredAt != nil {
		out.ExpiresAt = fc.ExpiredAt.Format(time.RFC3339)
	}
	// P0 多文件：附子文件清单（仅文件分享且存在子表行时）
	if s.shareSvc != nil && !fc.IsTextShare() {
		if items, lerr := s.shareSvc.ShareFiles(ctx, fc.Code); lerr == nil && len(items) > 1 {
			out.Files = make([]shareFileItem, 0, len(items))
			for _, it := range items {
				out.Files = append(out.Files, shareFileItem{Name: it.Name, SizeBytes: it.Size})
			}
		}
	}
	return out, nil
}

// ============ 工具 6/18：get_share_content ============

type getShareContentIn struct {
	Code string `json:"code" jsonschema:"分享码（8 位）、6 位取件码或自定义口令"`
}

type getShareContentOut struct {
	Code      string          `json:"code" jsonschema:"分享码"`
	Kind      string          `json:"kind" jsonschema:"分享类型：text 为文本，file 为文件"`
	Name      string          `json:"name" jsonschema:"展示名（文本取前 20 字符，文件取原始名）"`
	SizeBytes int64           `json:"size_bytes" jsonschema:"内容大小（字节）"`
	Text      string          `json:"text,omitempty" jsonschema:"文本分享正文"`
	Files     []shareFileItem `json:"files,omitempty" jsonschema:"文件清单（文件分享）"`
}

func (s *Service) getShareContent(ctx context.Context, in getShareContentIn, caller Caller) (getShareContentOut, error) {
	fc, err := s.resolveFileCode(ctx, in.Code)
	if err != nil {
		return getShareContentOut{}, fmt.Errorf("分享不存在或已过期")
	}
	// 已过期（时间）⇒ 拒绝（2026-10-26 审计 P2）。
	//
	// ⚠️ 此前本工具**只判次数、不判时间** ⇒ 同一条已过期分享，走
	// download_share_file 被拒、走 get_share_content 却能读走正文/文件清单。
	// 两个工具在注册表里都是交付面（ReadsShare）⇒ 口径必须一致。
	// （文案与 download_shareFile 逐字对齐，便于运维一眼对上。）
	if fc.IsExpired() {
		return getShareContentOut{}, fmt.Errorf("分享已过期（内容待清理任务释放，可先在管理端续期）")
	}
	// 次数已用完 ⇒ 拒绝（2026-10-24）：本工具**回传文本正文** = 一次内容交付。
	// MCP 的 share_text/share_file 支持 count 样式（按取件次数）⇒ 这类分享
	// 的「最多取 N 次」承诺必须在读回面也成立，否则上限形同虚设。
	// （拆分前 IsExpired 含次数语义 ⇒ 本工具同样拒；拆分后漏了这道闸。）
	if fc.IsCountExhausted() {
		return getShareContentOut{}, fmt.Errorf("取件次数已用完（分享已失效）")
	}
	out := getShareContentOut{Code: fc.Code, Kind: shareKind(fc), Name: displayFileName(fc), SizeBytes: fc.Size}
	if fc.IsTextShare() {
		out.Text = fc.Text
		return out, nil
	}
	if s.shareSvc != nil {
		if items, lerr := s.shareSvc.ShareFiles(ctx, fc.Code); lerr == nil && len(items) > 0 {
			out.Files = make([]shareFileItem, 0, len(items))
			for _, it := range items {
				out.Files = append(out.Files, shareFileItem{Name: it.Name, SizeBytes: it.Size})
			}
		}
	}
	return out, nil
}

// ============ 工具 7/18：download_share_file ============

type downloadShareFileIn struct {
	Code string `json:"code" jsonschema:"分享码（8 位）、6 位取件码或自定义口令"`
}

type downloadShareFileOut struct {
	FileName      string `json:"file_name" jsonschema:"文件名"`
	SizeBytes     int64  `json:"size_bytes" jsonschema:"文件大小（字节）"`
	ContentBase64 string `json:"content_base64" jsonschema:"文件内容（base64 编码）"`
}

func (s *Service) downloadShareFile(ctx context.Context, in downloadShareFileIn, caller Caller) (downloadShareFileOut, error) {
	fc, err := s.resolveFileCode(ctx, in.Code)
	if err != nil {
		return downloadShareFileOut{}, fmt.Errorf("分享不存在或已过期")
	}
	if fc.IsExpired() {
		return downloadShareFileOut{}, fmt.Errorf("分享已过期（内容待清理任务释放，可先在管理端续期）")
	}
	// 次数已用完 ⇒ 拒绝（2026-10-24）：本工具回传**文件内容** = 一次内容交付。
	// MCP 的 share_text/share_file 支持 count 样式（按取件次数）⇒ 这类分享
	// 的「最多取 N 次」承诺必须在读回面也成立，否则上限形同虚设。
	// （拆分前 IsExpired 含次数语义 ⇒ 本工具同样拒；拆分后漏了这道闸。）
	// 工具描述里的「不消耗取件次数」= 不**扣**次，不是无视上限。
	if fc.IsCountExhausted() {
		return downloadShareFileOut{}, fmt.Errorf("取件次数已用完（分享已失效）")
	}
	if fc.IsTextShare() {
		return downloadShareFileOut{}, fmt.Errorf("文本分享请用 get_share_content 读取正文")
	}
	// 多文件分享主路径非用户预期内容，拒绝并引导（清单见 get_share_content）
	if s.shareSvc != nil {
		if items, lerr := s.shareSvc.ShareFiles(ctx, fc.Code); lerr == nil && len(items) > 1 {
			return downloadShareFileOut{}, fmt.Errorf("多文件分享（%d 个）不支持整体下载，请用分享链接在 Web 端打包下载，或按文件分别处理", len(items))
		}
	}
	if s.storageSvc == nil {
		return downloadShareFileOut{}, fmt.Errorf("存储服务未注入")
	}
	path := fc.GetFilePath()
	size, serr := s.storageSvc.GetFileSize(ctx, path)
	if serr != nil {
		return downloadShareFileOut{}, fmt.Errorf("读取文件大小失败: %w", serr)
	}
	if size > s.mcpMaxFileSize() {
		return downloadShareFileOut{}, fmt.Errorf("文件 %d 字节超过 MCP 下载上限（%d 字节，PB_MCP_MAX_FILE_SIZE 可调）", size, s.mcpMaxFileSize())
	}
	data, derr := s.storageSvc.GetFile(ctx, path)
	if derr != nil {
		return downloadShareFileOut{}, fmt.Errorf("读取文件失败: %w", derr)
	}
	return downloadShareFileOut{
		FileName:      displayFileName(fc),
		SizeBytes:     int64(len(data)),
		ContentBase64: base64.StdEncoding.EncodeToString(data),
	}, nil
}

// ============ 工具 8/18：list_shares ============

type listSharesIn struct {
	Page     int    `json:"page,omitempty" jsonschema:"页码，默认 1"`
	PageSize int    `json:"page_size,omitempty" jsonschema:"每页条数，默认 20，上限 100"`
	Keyword  string `json:"keyword,omitempty" jsonschema:"搜索关键字（取件码/分享码/文件名）"`
}

type listSharesItem struct {
	Code       string `json:"code" jsonschema:"分享码"`
	PickupCode string `json:"pickup_code,omitempty" jsonschema:"6 位取件码"`
	Name       string `json:"name" jsonschema:"展示名"`
	SizeBytes  int64  `json:"size_bytes" jsonschema:"内容大小（字节）"`
	UsedCount  int    `json:"used_count" jsonschema:"已用取件次数"`
	ExpiresAt  string `json:"expires_at,omitempty" jsonschema:"过期时间（RFC3339；永久分享为空）"`
}

type listSharesOut struct {
	Total    int              `json:"total" jsonschema:"总条数"`
	Page     int              `json:"page" jsonschema:"当前页码"`
	PageSize int              `json:"page_size" jsonschema:"每页条数"`
	Items    []listSharesItem `json:"items" jsonschema:"分享记录列表"`
}

func (s *Service) listShares(ctx context.Context, in listSharesIn, caller Caller) (listSharesOut, error) {
	page := expireValue(in.Page, 1)
	pageSize := expireValue(in.PageSize, 20)
	if pageSize > 100 {
		pageSize = 100
	}
	files, total, err := s.fileCodeRepo.List(ctx, page, pageSize, in.Keyword)
	if err != nil {
		return listSharesOut{}, fmt.Errorf("查询失败: %w", err)
	}
	out := listSharesOut{Total: int(total), Page: page, PageSize: pageSize, Items: make([]listSharesItem, 0, len(files))}
	for _, f := range files {
		item := listSharesItem{
			Code:       f.Code,
			PickupCode: derefString(f.PickupCode),
			Name:       displayFileName(f),
			SizeBytes:  f.Size,
			UsedCount:  f.UsedCount,
		}
		if f.ExpiredAt != nil {
			item.ExpiresAt = f.ExpiredAt.Format(time.RFC3339)
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// ============ 工具 9/18：delete_share ============

type deleteShareIn struct {
	Code string `json:"code" jsonschema:"分享码（8 位）、6 位取件码或自定义口令"`
}

type deleteShareOut struct {
	Code string `json:"code" jsonschema:"已删除的分享码"`
	Name string `json:"name" jsonschema:"展示名"`
}

func (s *Service) deleteShare(ctx context.Context, in deleteShareIn, caller Caller) (deleteShareOut, error) {
	fc, err := s.resolveFileCode(ctx, in.Code)
	if err != nil {
		return deleteShareOut{}, fmt.Errorf("分享不存在: %s", in.Code)
	}
	if msg, missing := s.adminReady(); missing {
		return deleteShareOut{}, fmt.Errorf("%s", msg)
	}
	if err := s.adminSvc.DeleteShareByID(ctx, fc.ID); err != nil {
		return deleteShareOut{}, fmt.Errorf("删除失败: %w", err)
	}
	return deleteShareOut{Code: fc.Code, Name: displayFileName(fc)}, nil
}

// ============ 工具 10/18：get_system_status ============

type getSystemStatusOut struct {
	Version        string `json:"version" jsonschema:"服务版本"`
	TotalFiles     int64  `json:"total_files" jsonschema:"文件总数"`
	TotalUsers     int64  `json:"total_users" jsonschema:"用户总数"`
	TotalSizeBytes int64  `json:"total_size_bytes" jsonschema:"总占用（字节）"`
	TodayUploads   int64  `json:"today_uploads" jsonschema:"今日上传数"`
	ExpiredFiles   int64  `json:"expired_files" jsonschema:"过期文件数"`
}

func (s *Service) getSystemStatus(ctx context.Context, _ struct{}, caller Caller) (getSystemStatusOut, error) {
	if msg, missing := s.adminReady(); missing {
		return getSystemStatusOut{}, fmt.Errorf("%s", msg)
	}
	stats, err := s.adminSvc.SystemStats(ctx)
	if err != nil {
		return getSystemStatusOut{}, fmt.Errorf("查询失败: %w", err)
	}
	return getSystemStatusOut{
		Version:        s.version,
		TotalFiles:     stats.TotalFiles,
		TotalUsers:     stats.TotalUsers,
		TotalSizeBytes: stats.TotalSize,
		TodayUploads:   stats.TodayUploads,
		ExpiredFiles:   stats.ExpiredFiles,
	}, nil
}

// ============ 工具 11/18：get_storage_info ============

type getStorageInfoOut struct {
	StorageType     string  `json:"storage_type" jsonschema:"存储后端类型"`
	TotalSpaceBytes int64   `json:"total_space_bytes" jsonschema:"总空间（字节）"`
	UsedSpaceBytes  int64   `json:"used_space_bytes" jsonschema:"已用空间（字节）"`
	UsagePercent    float64 `json:"usage_percent" jsonschema:"使用率（%）"`
	FileCount       int64   `json:"file_count" jsonschema:"文件数"`
}

func (s *Service) getStorageInfo(ctx context.Context, _ struct{}, caller Caller) (getStorageInfoOut, error) {
	if msg, missing := s.adminReady(); missing {
		return getStorageInfoOut{}, fmt.Errorf("%s", msg)
	}
	st, err := s.adminSvc.StorageStatus(ctx)
	if err != nil {
		return getStorageInfoOut{}, fmt.Errorf("查询失败: %w", err)
	}
	return getStorageInfoOut{
		StorageType:     st.StorageType,
		TotalSpaceBytes: st.TotalSpace,
		UsedSpaceBytes:  st.UsedSpace,
		UsagePercent:    st.UsagePercent,
		FileCount:       st.FileCount,
	}, nil
}

// ============ 工具 12/18：list_users ============

type listUsersIn struct {
	Page     int `json:"page,omitempty" jsonschema:"页码，默认 1"`
	PageSize int `json:"page_size,omitempty" jsonschema:"每页条数，默认 20"`
}

type listUsersItem struct {
	ID       uint   `json:"id" jsonschema:"用户 ID"`
	Username string `json:"username" jsonschema:"用户名"`
	Email    string `json:"email" jsonschema:"邮箱"`
	Status   string `json:"status" jsonschema:"状态"`
}

type listUsersOut struct {
	Total    int             `json:"total" jsonschema:"总条数"`
	Page     int             `json:"page" jsonschema:"当前页码"`
	PageSize int             `json:"page_size" jsonschema:"每页条数"`
	Items    []listUsersItem `json:"items" jsonschema:"用户列表"`
}

func (s *Service) listUsers(ctx context.Context, in listUsersIn, caller Caller) (listUsersOut, error) {
	if msg, missing := s.adminReady(); missing {
		return listUsersOut{}, fmt.Errorf("%s", msg)
	}
	page := expireValue(in.Page, 1)
	pageSize := expireValue(in.PageSize, 20)
	users, total, err := s.adminSvc.Users(ctx, page, pageSize)
	if err != nil {
		return listUsersOut{}, fmt.Errorf("查询失败: %w", err)
	}
	out := listUsersOut{Total: int(total), Page: page, PageSize: pageSize, Items: make([]listUsersItem, 0, len(users))}
	for _, u := range users {
		//nolint:staticcheck // S1016 的建议在此不可行：Go 的结构体转换要求两侧
		// 字段名/类型/**tag** 都相同，而本类型带 jsonschema tag（输出契约）、
		// 源类型 UserRow 不带 ⇒ 转不进去。逐字段赋值保持两套契约各自独立。
		out.Items = append(out.Items, listUsersItem{ID: u.ID, Username: u.Username, Email: u.Email, Status: u.Status})
	}
	return out, nil
}

// ============ 工具 13/18：cleanup_expired ============

type cleanupExpiredOut struct {
	CleanedCount int64 `json:"cleaned_count" jsonschema:"已清理的过期分享数"`
	FreedBytes   int64 `json:"freed_bytes" jsonschema:"释放的字节数"`
}

func (s *Service) cleanupExpired(ctx context.Context, _ struct{}, caller Caller) (cleanupExpiredOut, error) {
	if msg, missing := s.adminReady(); missing {
		return cleanupExpiredOut{}, fmt.Errorf("%s", msg)
	}
	n, freed, err := s.adminSvc.CleanExpired(ctx)
	if err != nil {
		return cleanupExpiredOut{}, fmt.Errorf("清理失败: %w", err)
	}
	return cleanupExpiredOut{CleanedCount: n, FreedBytes: freed}, nil
}

// ============ 工具 14/18：federation_status ============

type federationStatusOut struct {
	Enabled    bool     `json:"enabled" jsonschema:"联邦是否启用"`
	NodeID     string   `json:"node_id,omitempty" jsonschema:"本节点 ID"`
	Healthy    bool     `json:"healthy" jsonschema:"最近心跳是否正常"`
	Registries []string `json:"registries" jsonschema:"注册中心地址列表"`
}

func (s *Service) federationStatus(ctx context.Context, _ struct{}, caller Caller) (federationStatusOut, error) {
	fed := s.currentFederation()
	if fed == nil {
		return federationStatusOut{Enabled: false, Registries: []string{}}, nil
	}
	st := fed.Status()
	// 适配器在联邦未启用时返回 Enabled:false（非 nil 接口包 nil 实例）
	//nolint:staticcheck // S1016 同上：federationStatusOut 带 jsonschema tag、
	// FederationStatus 不带 ⇒ 结构体转换不合法（Go 要求 tag 相同）。
	return federationStatusOut{
		Enabled:    st.Enabled,
		NodeID:     st.NodeID,
		Healthy:    st.Healthy,
		Registries: st.Registries,
	}, nil
}

// ============ 工具 15/18：federation_resolve ============

type federationResolveIn struct {
	Code string `json:"code" jsonschema:"分享码（8 位）或取件口令"`
}

type federationResolveOut struct {
	Found         bool   `json:"found" jsonschema:"是否在联邦内找到源节点"`
	NodeID        string `json:"node_id,omitempty" jsonschema:"源节点 ID"`
	URL           string `json:"url,omitempty" jsonschema:"源节点对外地址"`
	Name          string `json:"name,omitempty" jsonschema:"源节点名称"`
	ExpiresAt     string `json:"expires_at,omitempty" jsonschema:"路由过期时间（RFC3339）"`
	SizeHintBytes int64  `json:"size_hint_bytes,omitempty" jsonschema:"内容大小提示（字节）"`
}

func (s *Service) federationResolve(ctx context.Context, in federationResolveIn, caller Caller) (federationResolveOut, error) {
	fed := s.currentFederation()
	if fed == nil {
		return federationResolveOut{}, fmt.Errorf("联邦未启用")
	}
	info, rerr := fed.Resolve(in.Code)
	if rerr != nil {
		return federationResolveOut{}, fmt.Errorf("联邦解析失败: %w", rerr)
	}
	if info == nil {
		return federationResolveOut{Found: false}, nil
	}
	return federationResolveOut{
		Found:         true,
		NodeID:        info.NodeID,
		URL:           info.URL,
		Name:          info.Name,
		ExpiresAt:     info.ExpiresAt.Format(time.RFC3339),
		SizeHintBytes: info.SizeHint,
	}, nil
}

// ============ 工具 16/18：p2p_peers ============

type p2pPeersOut struct {
	Peers []p2pPeerItem `json:"peers" jsonschema:"在线对端列表（名录 opt-in，零敏感）"`
}

type p2pPeerItem struct {
	NodeID      string `json:"node_id" jsonschema:"对端节点 ID"`
	Name        string `json:"name" jsonschema:"对端名称"`
	URL         string `json:"url,omitempty" jsonschema:"对端对外地址（NAT 后可能为空）"`
	BoxPub      string `json:"box_pub" jsonschema:"对端 Box 公钥指纹"`
	OnlineSince int64  `json:"online_since" jsonschema:"上线时刻（Unix 秒）"`
}

func (s *Service) p2pPeers(ctx context.Context, _ struct{}, caller Caller) (p2pPeersOut, error) {
	p2p := s.currentP2p()
	if p2p == nil {
		return p2pPeersOut{Peers: []p2pPeerItem{}}, nil
	}
	peers, perr := p2p.P2pPeers(ctx)
	if perr != nil {
		return p2pPeersOut{}, fmt.Errorf("名录查询失败: %w", perr)
	}
	out := p2pPeersOut{Peers: make([]p2pPeerItem, 0, len(peers))}
	for _, p := range peers {
		//nolint:staticcheck // S1016 同上：p2pPeerItem 带 jsonschema tag，
		// P2pPeerInfo 的 url 还带 omitempty 差异 ⇒ 结构体转换不合法。
		out.Peers = append(out.Peers, p2pPeerItem{
			NodeID: p.NodeID, Name: p.Name, URL: p.URL, BoxPub: p.BoxPub, OnlineSince: p.OnlineSince,
		})
	}
	return out, nil
}

// ============ 工具 17/18：p2p_invites ============

type p2pInvitesOut struct {
	Invites []p2pInviteItem `json:"invites" jsonschema:"待收邀请列表（TTL 10 分钟）"`
}

type p2pInviteItem struct {
	ID        string `json:"id" jsonschema:"邀请 id（p2p_accept 用）"`
	Code      string `json:"code" jsonschema:"邀请口令"`
	FromName  string `json:"from_name" jsonschema:"来源实例名称"`
	Note      string `json:"note,omitempty" jsonschema:"附言"`
	ExpiresIn int64  `json:"expires_in" jsonschema:"剩余有效秒数"`
}

func (s *Service) p2pInvites(ctx context.Context, _ struct{}, caller Caller) (p2pInvitesOut, error) {
	p2p := s.currentP2p()
	if p2p == nil {
		return p2pInvitesOut{Invites: []p2pInviteItem{}}, nil
	}
	invites, ierr := p2p.P2pInvites(ctx)
	if ierr != nil {
		return p2pInvitesOut{}, fmt.Errorf("待收邀请查询失败: %w", ierr)
	}
	out := p2pInvitesOut{Invites: make([]p2pInviteItem, 0, len(invites))}
	for _, it := range invites {
		//nolint:staticcheck // S1016 同上：p2pInviteItem 带 jsonschema tag
		// 与源类型的 omitempty 差异 ⇒ 结构体转换不合法。
		out.Invites = append(out.Invites, p2pInviteItem{
			ID: it.ID, Code: it.Code, FromName: it.FromName, Note: it.Note, ExpiresIn: it.ExpiresIn,
		})
	}
	return out, nil
}

// ============ 工具 18/18：p2p_accept ============

type p2pAcceptIn struct {
	InviteID string `json:"invite_id" jsonschema:"待收邀请 id（经 p2p_invites 查询）"`
}

type p2pAcceptOut struct {
	InviteID  string `json:"invite_id" jsonschema:"已认领的邀请 id"`
	Code      string `json:"code" jsonschema:"邀请口令（信封已出箱）"`
	FromName  string `json:"from_name" jsonschema:"来源实例名称"`
	ExpiresIn int64  `json:"expires_in" jsonschema:"剩余有效秒数"`
}

func (s *Service) p2pAccept(ctx context.Context, in p2pAcceptIn, caller Caller) (p2pAcceptOut, error) {
	p2p := s.currentP2p()
	if p2p == nil {
		return p2pAcceptOut{}, fmt.Errorf("联邦面未装配")
	}
	id := strings.TrimSpace(in.InviteID)
	if id == "" {
		return p2pAcceptOut{}, fmt.Errorf("invite_id 不能为空（经 p2p_invites 查询）")
	}
	inv, aerr := p2p.P2pAccept(ctx, id)
	if aerr != nil {
		return p2pAcceptOut{}, fmt.Errorf("认领失败: %w", aerr)
	}
	return p2pAcceptOut{
		InviteID: inv.ID, Code: inv.Code, FromName: inv.FromName, ExpiresIn: inv.ExpiresIn,
	}, nil
}

// ============ 工具层公共辅助 ============

// shareCreatedOut 组装分享创建的结构化出参。
//
// display 为展示名（文本分享取正文前 20 字符，文件分享取文件名）；
// 直链只在无密码文件分享时给出（密码分享须先经取件页验密，直链闸门会拒绝）。
func (s *Service) shareCreatedOut(created ShareCreated, display, fileName string, size int64, hasPassword, isFile bool) shareCreatedOut {
	out := shareCreatedOut{
		Code:        created.Code,
		PickupCode:  created.PickupCode,
		ShareURL:    created.FullShareURL,
		HasPassword: hasPassword,
		FileName:    fileName,
		SizeBytes:   size,
	}
	if display != "" {
		out.Markdown = fmt.Sprintf("[%s](%s)", escapeMD(display), created.FullShareURL)
	}
	if isFile && !hasPassword {
		out.DownloadURL = anonymousDownloadURL(created.FullShareURL, created.Code)
	}
	return out
}

// shareKind 分享类型（text/file）。
func shareKind(fc *model.FileCode) string {
	if fc.IsTextShare() {
		return "text"
	}
	return "file"
}

// derefString 取指针字符串值（nil/空 → 空串）。
func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// expireValue 过期/分页数值归一：非正值回退默认。
func expireValue(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// normalizeExpireStyle 过期样式归一 + 枚举校验（空 → def；非法值 → 明确错误，
// 让模型能据报错自纠，而不是被静默改写成 day）。
//
// 合法集合直接用 pkg/utils 的单一真相源（IsValidExpireStyle）——本文件原有一份
// 独立白名单，漏了 "count"（按取件次数过期），而 share 域的合法集合里有
// ⇒ 同一能力在 MCP 入口被判非法、网页入口可用（2026-10-22 审计）。
func normalizeExpireStyle(style string) (string, error) {
	if style == "" {
		return "day", nil
	}
	if !utils.IsValidExpireStyle(style) {
		return "", fmt.Errorf("expire_style 非法：%q（可用：%s）", style, strings.Join(utils.AllExpireStyles(), "/"))
	}
	return style, nil
}

// hashPassword 明文口令 → 哈希（空口令 → 空哈希 = 不启用密码保护）。
func hashPassword(pw string) (string, error) {
	if pw == "" {
		return "", nil
	}
	hash, err := utils.HashPassword(pw)
	if err != nil {
		return "", fmt.Errorf("密码处理失败")
	}
	return hash, nil
}
