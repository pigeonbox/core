package model

import (
	"strings"
	"time"

	"gorm.io/gorm"
)

// 分享管控状态（治理 2026-10-03）。与软删除正交：
//   - normal        正常（默认；老库 AutoMigrate 补列后全部回填 normal）
//   - blocked       管理员禁用（不可取件/下载，可恢复，记录留证）
//   - pending_review 内容待审核（moderation 命中且策略为 pending 时置入，审核通过恢复 normal）
const (
	StatusNormal        = "normal"
	StatusBlocked       = "blocked"
	StatusPendingReview = "pending_review"
)

// ValidShareStatus 状态机合法取值白名单（写入口统一校验）。
func ValidShareStatus(s string) bool {
	switch s {
	case StatusNormal, StatusBlocked, StatusPendingReview:
		return true
	}
	return false
}

// FileCode 文件代码模型
type FileCode struct {
	gorm.Model
	Code         string `gorm:"uniqueIndex;size:255" json:"code"`
	UUIDFileName string `gorm:"size:255" json:"uuid_file_name"`
	FilePath     string `gorm:"size:255" json:"file_path"`
	Size         int64  `gorm:"default:0" json:"size"`
	Text         string `gorm:"type:text" json:"text"`
	// Note 文件备注（可空）：随分享展示给取件人（2026-10-12，文件+文本一并投递）
	Note         string     `gorm:"type:text" json:"note"`
	ExpiredAt    *time.Time `gorm:"index:idx_file_codes_status_expired,priority:2" json:"expired_at"`
	ExpiredCount int        `gorm:"default:0" json:"expired_count"` // 剩余可取次数：-1=无限, 0=已耗尽, >0=剩余
	UsedCount    int        `gorm:"default:0" json:"used_count"`
	// Status 管控状态，默认 normal；blocked/pending_review 拒绝取件（索引覆盖管理端按状态筛选）
	Status string `gorm:"size:20;default:'normal';index:idx_file_codes_status_expired,priority:1" json:"status"`

	FileHash  string `gorm:"size:64" json:"file_hash"`
	IsChunked bool   `gorm:"default:false" json:"is_chunked"`
	UploadID  string `gorm:"size:36" json:"upload_id"`

	// 新增：用户认证相关字段
	UserID       *uint  `gorm:"index" json:"user_id"`                           // 上传用户ID，为null表示匿名上传
	UploadType   string `gorm:"size:20;default:'anonymous'" json:"upload_type"` // anonymous, authenticated
	RequireAuth  bool   `gorm:"default:false" json:"require_auth"`              // 是否需要密码才能下载
	PasswordHash string `gorm:"size:255" json:"-"`                              // 取件密码的 bcrypt 哈希（json:"-" 不外泄）
	OwnerIP      string `gorm:"size:45" json:"owner_ip"`                        // 上传者IP地址
	// Encrypted 端到端客户端加密（P1 E2E）：true 时存储的是密文，密钥经分享链接
	// #fragment 传递、从不落服务端；服务端零知识（预览/服务端 zip 打包不适用）
	Encrypted bool `gorm:"default:false" json:"encrypted"`

	// 取件追踪（软删除字段 gorm.Model.DeletedAt 已自带）
	ViewerIP       string     `gorm:"size:45" json:"viewer_ip"`      // 最近一次取件人IP
	ViewerAt       *time.Time `json:"viewer_at"`                     // 最近一次取件时间
	ViewerCount    int        `gorm:"default:0" json:"viewer_count"` // 累计取件次数
	LastNotifiedAt *time.Time `json:"last_notified_at"`              // 最近一次给 owner 发通知的时间（用于去重）

	// PickupCode 6 位取件码（2026-10-08 起落库持久化，"只保留 6 位码"呈现）：
	//   - 本列为真相源；Redis/memkv 映射降级为加速缓存（TTL=分享过期时间），
	//     单机内存模式重启后经 GetByPickupCode 回退解析，取件码不再失联
	//   - 永久分享同样铸造（KV 无法表达永久 TTL，DB 无此约束）
	//   - nil=未铸造（E2E 密文分享/历史数据）；可空唯一索引允许多行 NULL
	PickupCode *string `gorm:"uniqueIndex;size:8" json:"pickup_code,omitempty"`
}

// PickupCodeLength 取件码长度（6 位）——**唯一真相源**。
//
// 此前三处各写一份（app/anonymous 的 codeLength、app/share 的 pickupCodeLength、
// app/mcp 的字面量 6）⇒ 任一处改长度即静默漂移（分享域兜底失效 / MCP 解析失效）。
const PickupCodeLength = 6

// IsNotReady 判定记录是否为「未完成上传」的占位分享：既无文件路径、也非文本分享。
//
// 与匿名通道的判定式逐字一致（!IsTextShare() && GetFilePath() == ""）。
// 判定式必须**只有一处**（分散在多域会各自漂移 ⇒ 同一份占位分享在不同端点
// 给出不同结论）。
func IsNotReady(fc *FileCode) bool {
	return fc != nil && !fc.IsTextShare() && fc.GetFilePath() == ""
}

// IsExpired 检查是否**时间**过期。
//
// ⚠️ 2026-10-24 起本方法**不再**把「取件次数耗尽」算作过期：
// 此前 ExpiredCount==0 ⇒ IsExpired()==true ⇒ 两条通道对同一条分享给出相反结论：
//   - 分享页通道（select → download）：扣次发生在**下载请求内**，末次取件能拿到文件；
//   - 取件码通道（retrieve → download）：retrieve 先把次数扣到 0 ⇒ 随后的下载被判
//     「已过期」⇒ **末次取件必然 404，文件永远拿不到**（2026-10-24 审计）。
//
// 「次数耗尽」有自己的判定（IsCountExhausted）与错误码（10003 / 20007），
// 不再伪装成「不存在/已过期」。
func (f *FileCode) IsExpired() bool {
	return f.ExpiredAt != nil && f.ExpiredAt.Before(time.Now())
}

// IsCountExhausted 取件次数是否已耗尽（ExpiredCount 语义：-1=无限, 0=已耗尽, >0=剩余）。
//
// 与 IsExpired（时间）正交：次数耗尽 ⇒ 不能再发起**新的取件**，
// 但已扣次的那次取件（同一取件会话内）仍应可完成交付。
func (f *FileCode) IsCountExhausted() bool {
	return f.ExpiredCount == 0
}

// IsPickupAvailable 是否还能发起新的取件（时间未过期 且 次数未耗尽）。
func (f *FileCode) IsPickupAvailable() bool {
	return !f.IsExpired() && !f.IsCountExhausted()
}

// GetFilePath 获取文件路径（存储相对路径，相对存储根）。
//
// 口径统一（2026-10-26 兼容层退役）：file_path 恒为「含唯一文件名的完整相对路径」，
// 所有写入通道（直传/分片/预签名/匿名取件/多文件/本地导入）一致。此前本方法并存
// 两种形态——「目录 + UUIDFileName」与「file_path 直含完整路径」——靠 strings.Contains
// 猜走哪条分支；同一列两种语义导致「双重路径」拼装（见 anonymous.BindFilePath 的
// 清空兜底注释），且匿名占位记录（file_path 空、uuid_file_name 为展示名）会拼出
// 不存在的路径。列语义既已统一，本方法退化为单一取值。
func (f *FileCode) GetFilePath() string {
	return strings.TrimSpace(f.FilePath)
}

// IsBlockedShare 分享是否处于管控拒绝态（blocked / pending_review）
func (f *FileCode) IsBlockedShare() bool {
	return f.Status == StatusBlocked || f.Status == StatusPendingReview
}

// IsTextShare 判定是否纯文本分享：Text 非空且无文件路径。
//
// 回归要点（P0）：文件分享会把原始文件名存进 Text 字段，因此仅凭
// Text != "" 判定会把文件分享误判为文本——文件下载曾被文本分支拦截，
// 返回文件名字符串而非文件内容。判定逻辑此前在 app/share 包级函数，
// 多域（anonymous/mcp/presign）需要时各自 import share——下沉到 model
// 作为领域事实，消费方零跨域依赖。
func (f *FileCode) IsTextShare() bool {
	return f != nil && f.Text != "" && f.FilePath == ""
}

// DisplayName 返回分享条目的展示文件名（下载 Content-Disposition、管理/用户
// 列表、取件元信息等所有呈现位统一从本方法取名）。
//
// 口径统一（2026-10-26 兼容层退役）：Text（原始文件名）→ UUIDFileName（存储名）
// → 路径末段。此前本方法以 UUIDFileName 优先，而子表 FileCodeFile.DisplayName
// 以 FileName 优先——同一份文件在管理端列表与取件页显示两个名字（前者显示
// "56781532-….txt"，后者显示 "m1.txt"）。Prefix+Suffix 分支是旧直传布局的
// 原始名拆分，库中已无任何行使用，随死列一并退役。
// 纯文本分享（Text=正文且无文件路径）返回空串——文本没有文件名语义,兜底由调用方定。
func (f *FileCode) DisplayName() string {
	if f == nil {
		return ""
	}
	if f.IsTextShare() {
		return ""
	}
	if f.Text != "" {
		return f.Text
	}
	if f.UUIDFileName != "" {
		return f.UUIDFileName
	}
	return baseNameOf(f.FilePath)
}

// baseNameOf 取相对路径末段（展示名兜底）。rel 恒用 "/" 分隔，不走
// filepath.Base（Windows 下不认 "/"）。
func baseNameOf(rel string) string {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return ""
	}
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		return rel[i+1:]
	}
	return rel
}

// FileCodeQuery 管理端文件列表过滤条件（DAO ListWithFilter 消费）。
// 全字段可选，零值 = 不过滤。
type FileCodeQuery struct {
	Keyword       string // 模糊匹配 code/pickup_code/uuid_file_name/text/file_path
	UserID        *uint  // 上传者
	UploadType    string // anonymous/authenticated/presign_*
	OwnerIP       string // 上传者 IP（滥用定位）
	Status        string // normal/blocked/pending_review
	MinSize       *int64 // 大小区间（字节）
	MaxSize       *int64
	CreatedAfter  *time.Time // 创建时间区间
	CreatedBefore *time.Time
	Expired       *bool // true=仅过期(时间或次数)，false=仅未过期
	// Health 健康洞察过滤（2026-10-07）：active/expired/expiring_soon/never_picked/forever，
	// 空串=不过滤；口径见 dao.applyHealthFilter（与 CountByHealth 同源）
	Health string
	// Deleted 软删筛选：""=默认仅存活 / "only"=仅回收站(已软删) / "all"=全部。
	// 回收站视图用 "only"；配合 status/keyword 等既有筛选叠加。
	Deleted  string // "", "only", "all"
	Page     int
	PageSize int
}

// FileCodeUpdate 文件代码更新数据
type FileCodeUpdate struct {
	gorm.Model
	ExpiredAt    *time.Time `json:"expired_at"`
	ExpiredCount *int       `json:"expired_count"`
	UsedCount    *int       `json:"used_count"`
	RequireAuth  *bool      `json:"require_auth"`
	OwnerIP      *string    `json:"owner_ip"`
}

// FileCodeStats 文件统计查询结果
type FileCodeStats struct {
	gorm.Model
	TotalFiles     int64 `json:"total_files"`
	TotalSize      int64 `json:"total_size"`
	TodayUploads   int64 `json:"today_uploads"`
	TodayDownloads int64 `json:"today_downloads"`
	ExpiredFiles   int64 `json:"expired_files"`
	AnonymousFiles int64 `json:"anonymous_files"`
	UserFiles      int64 `json:"user_files"`
}
