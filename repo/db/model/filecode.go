package model

import (
	"path/filepath"
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
	Prefix       string `gorm:"size:255" json:"prefix"`
	Suffix       string `gorm:"size:255" json:"suffix"`
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

// IsExpired 检查是否过期
func (f *FileCode) IsExpired() bool {
	// 检查时间过期
	if f.ExpiredAt != nil && f.ExpiredAt.Before(time.Now()) {
		return true
	}

	// 检查次数过期
	// ExpiredCount 语义：-1=无限(不过期), 0=已耗尽(过期), >0=剩余(不过期)
	if f.ExpiredCount == 0 {
		return true
	}

	return false
}

// GetFilePath 获取文件路径
func (f *FileCode) GetFilePath() string {
	// 新格式：FilePath（目录）+ UUIDFileName（文件名）
	if f.FilePath != "" && f.UUIDFileName != "" {
		// 检查FilePath是否已经包含了文件名（兼容性处理）
		// 如果FilePath已经是完整路径（包含文件扩展名），直接返回
		if strings.Contains(f.FilePath, f.UUIDFileName) {
			return f.FilePath // 旧格式：FilePath包含完整路径
		}
		// 新格式：组合目录和文件名
		return filepath.Join(f.FilePath, f.UUIDFileName)
	}

	// 兼容旧格式：file_path 字段直接包含完整的相对路径
	if f.FilePath != "" {
		return f.FilePath
	}

	return ""
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
// 解析顺序：UUIDFileName(chunk 等通道落库名) → Prefix+Suffix(旧直传布局的原始
// 名拆分) → Text(直传/E2E 通道契约:原始文件名存 Text)。2026-10-07 修复:直传
// 分享三处名字段全空,DownloadFile 曾给出 Content-Disposition filename=""
// (浏览器无法命名下载文件),管理端文件名回退显示成分享码。
// 纯文本分享(Text=正文且无文件路径)返回空串——文本没有文件名语义,兜底由调用方定。
func (f *FileCode) DisplayName() string {
	if f == nil {
		return ""
	}
	if f.UUIDFileName != "" {
		return f.UUIDFileName
	}
	if name := f.Prefix + f.Suffix; name != "" {
		return name
	}
	if !f.IsTextShare() && f.Text != "" {
		return f.Text
	}
	return ""
}

// FileCodeQuery 管理端文件列表过滤条件（DAO ListWithFilter 消费）。
// 全字段可选，零值 = 不过滤。
type FileCodeQuery struct {
	Keyword       string // 模糊匹配 code/prefix/suffix/uuid_file_name/text
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
