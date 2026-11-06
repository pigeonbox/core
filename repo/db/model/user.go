package model

import (
	"time"

	"gorm.io/gorm"
)

type User struct {
	gorm.Model
	Username      string     `gorm:"uniqueIndex;size:50" json:"username"`
	Email         string     `gorm:"uniqueIndex;size:100" json:"email"`
	PasswordHash  string     `gorm:"size:128" json:"-"`                      // 密码哈希，不返回给前端
	Nickname      string     `gorm:"size:50" json:"nickname"`                // 用户昵称
	Avatar        string     `gorm:"size:255" json:"avatar"`                 // 头像URL
	Role          string     `gorm:"size:20;default:'user'" json:"role"`     // admin, user
	Status        string     `gorm:"size:20;default:'active'" json:"status"` // active, inactive, banned
	EmailVerified bool       `gorm:"default:false" json:"email_verified"`    // 邮箱是否验证
	LastLoginAt   *time.Time `json:"last_login_at"`                          // 最后登录时间
	LastLoginIP   string     `gorm:"size:45" json:"last_login_ip"`           // 最后登录IP
	OidcSub       string     `gorm:"index;size:128" json:"-"`                // OIDC subject（P2 SSO；空=非 OIDC）
	// SessionEpoch 会话纪元：改密/封禁/降权时 +1，使全部已签发 JWT 即时失效
	// （2026-10-05 审计 P2：此前封禁/重置密码后旧 JWT 仍有效至自然过期）。
	SessionEpoch int `gorm:"default:0" json:"-"`

	// ---- 账号与信任（5.2.0）----
	//
	// TotpSecret TOTP 共享密钥（base32，无填充）。落库为**明文**，理由：
	// 它需要一把「比 JWT secret 更稳定」的密钥来加密；而 JWT secret 是允许
	// 运维更换的配置（换了全部会话失效是设计预期）⇒ 若用它加密，换一次
	// secret 就等于**全员二验静默失效**（只能靠恢复码，而恢复码也没了）。
	// 折中后选明文：能读这张表的人本来就能读全部分享内容与对象存储凭据，
	// TOTP 密钥不构成新的信任边界。
	//
	// 未启用时也可能有值：绑定第一步写入、第二步没验过就放弃 ⇒ 留着供重试。
	TotpSecret string `gorm:"size:64" json:"-"`
	// TotpEnabled 是否已启用（绑定第二步验码成功才 true）。
	TotpEnabled bool `gorm:"default:false" json:"totp_enabled"`
	// TotpRecoveryCodes 一次性恢复码的 bcrypt 哈希（JSON 数组字符串）。
	// 只存哈希：库被读走也换不回可用恢复码。
	TotpRecoveryCodes string `gorm:"size:1024" json:"-"`

	// 用户上传统计
	TotalUploads    int   `gorm:"default:0" json:"total_uploads"`     // 总上传次数
	TotalDownloads  int   `gorm:"default:0" json:"total_downloads"`   // 总下载次数
	TotalStorage    int64 `gorm:"default:0" json:"total_storage"`     // 总存储大小(字节)
	MaxUploadSize   int64 `gorm:"default:0" json:"max_upload_size"`   // 最大单次上传大小(字节)，0表示使用系统默认
	MaxStorageQuota int64 `gorm:"default:0" json:"max_storage_quota"` // 最大存储配额(字节)，0表示无限制
}

type UserResp struct {
	ID              uint       `json:"id"`
	Username        string     `json:"username"`
	Email           string     `json:"email"`
	Nickname        string     `json:"nickname"`
	Avatar          string     `json:"avatar"`
	Role            string     `json:"role"`
	Status          string     `json:"status"`
	EmailVerified   bool       `json:"email_verified"`
	LastLoginAt     *time.Time `json:"last_login_at"`
	LastLoginIP     string     `json:"last_login_ip"`
	TotalUploads    int        `json:"total_uploads"`
	TotalDownloads  int        `json:"total_downloads"`
	TotalStorage    int64      `json:"total_storage"`
	MaxUploadSize   int64      `json:"max_upload_size"`
	MaxStorageQuota int64      `json:"max_storage_quota"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

func (u *User) ToResp() *UserResp {
	return &UserResp{
		ID:              u.ID,
		Username:        u.Username,
		Email:           u.Email,
		Nickname:        u.Nickname,
		Avatar:          u.Avatar,
		Role:            u.Role,
		Status:          u.Status,
		EmailVerified:   u.EmailVerified,
		LastLoginAt:     u.LastLoginAt,
		LastLoginIP:     u.LastLoginIP,
		TotalUploads:    u.TotalUploads,
		TotalDownloads:  u.TotalDownloads,
		TotalStorage:    u.TotalStorage,
		MaxUploadSize:   u.MaxUploadSize,
		MaxStorageQuota: u.MaxStorageQuota,
		CreatedAt:       u.CreatedAt,
		UpdatedAt:       u.UpdatedAt,
	}
}

// UserQuery 用户查询条件
type UserQuery struct {
	ID       *uint  `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
	Role     string `json:"role"`
	Status   string `json:"status"`
	Search   string `json:"search"` // 模糊搜索用户名或邮箱
	Limit    int    `json:"limit"`
	Offset   int    `json:"offset"`
}

// UserStats 用户统计查询结果
type UserStats struct {
	UserID         uint  `json:"user_id"`
	TotalUploads   int   `json:"total_uploads"`
	TotalDownloads int   `json:"total_downloads"`
	TotalStorage   int64 `json:"total_storage"`
	FileCount      int   `json:"file_count"`
}
