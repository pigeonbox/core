package model

import (
	"time"

	"gorm.io/gorm"
)

// UserSession 一条**登录会话**（5.2.0）。
//
// # 为什么必须建这张表
//
// 「退出所有设备」原本已有 epoch 机制（改密/封禁即 +1 让全部旧 JWT 失效），
// 但**「我有哪些设备在线」无法从无状态 JWT 回答**——JWT 只在客户端手里，
// 服务端不留痕。所以登录时登记一行，认证中间件按 claims 里的 sid 回查是否
// 已撤销；「退出所有设备」= 把这些行标记撤销（并配 epoch+1 兜住升级前
// 签发、没有 sid 的老令牌）。
//
// # 与 API Key 的分工（语义必须一致，否则用户会误判）
//
// 会话 = 人机登录（浏览器/客户端），可整体作废；
// API Key = 长期程序凭据，**不受「退出所有设备」影响**，须另行吊销
// ——与 admin 口令重置的既有语义一致（那边也只失效 JWT、保留 Key）。
//
// # 为什么不做「每请求都记账」
//
// LastSeenAt 只在**登录与刷新令牌**时更新，不在每个请求上写库：
// 会话表是审计面不是计费面，每请求一写会把最热的认证路径变成写放大。
type UserSession struct {
	gorm.Model
	UserID uint `gorm:"index;not null" json:"user_id"`
	// SessionID 会话标识（随机，写进 JWT 的 sid）。唯一索引 ⇒ 一个 sid 只可能
	// 对应一行；重复登录（同一设备再登一次）是**新的一行**，不是复用。
	SessionID  string    `gorm:"uniqueIndex;size:36;not null" json:"-"`
	IP         string    `gorm:"size:45" json:"ip"`
	UserAgent  string    `gorm:"size:255" json:"user_agent"`
	LastSeenAt time.Time `json:"last_seen_at"`
	// RevokedAt 撤销时间（空 = 仍然有效）。软撤销保留审计留痕。
	RevokedAt *time.Time `json:"revoked_at"`
}

// PasswordResetToken 邮箱找回密码的一次性令牌（5.2.0）。
//
// 只存 sha256 哈希：邮件里的明文 token 泄漏途径太多（邮件服务器日志、
// 转发、截图），库内再留一份明文等于把风险翻倍。
type PasswordResetToken struct {
	gorm.Model
	UserID    uint       `gorm:"index;not null" json:"user_id"`
	TokenHash string     `gorm:"uniqueIndex;size:64;not null" json:"-"`
	ExpiresAt time.Time  `gorm:"index" json:"expires_at"`
	UsedAt    *time.Time `json:"used_at"`
}
