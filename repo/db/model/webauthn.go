package model

import (
	"time"

	"gorm.io/gorm"
)

// WebAuthnCredential WebAuthn/passkey 凭证（core#160 管理员 Passkey 登记）。
//
// 只存「验证断言所需的最小凭证记录」（RFC 9368 Credential Record 子集）：
// 公钥 + 签名计数 + 归属用户。挑战等短命中间态**不落库**——challenge 是
// 一次性秘密，落库只会给枚举攻击送素材（进程内 TTL 存取，见 app/admin/webauthn.go）。
type WebAuthnCredential struct {
	gorm.Model
	// UserID 归属用户（users.id）。登记面目前只有管理员
	//（/admin/webauthn/register/*），但表按通用归属建 ⇒ 后续 user 域复用零迁移。
	UserID uint `gorm:"index;not null" json:"user_id"`
	// CredentialID 凭证 ID（base64url 无填充）。唯一索引：同一张 passkey
	// 重复登记在落库前即被拦（换别名重登也只该有一行）。
	CredentialID string `gorm:"uniqueIndex;size:128" json:"-"`
	// PublicKey COSE_Key 格式公钥（CBOR 字节）。编解码全部由
	// go-webauthn/webauthn 库负责，本仓不手写协议格式。
	PublicKey []byte `gorm:"not null" json:"-"`
	// SignCount 签名计数（克隆检测；0 = 计数器不支持或已触发回滚置零）。
	SignCount uint32 `gorm:"default:0" json:"sign_count"`
	// Transports 传输方式（逗号分隔，如 "usb,internal"）——仅展示用。
	Transports string `gorm:"size:64" json:"transports"`
	// AAGUID 认证器 AAGUID（hex；全零 = 无 attestation）。
	AAGUID string `gorm:"size:32" json:"aaguid"`
	// Name 用户给这把 passkey 起的别名（管理台展示）。
	Name string `gorm:"size:100" json:"name"`
	// LastUsedAt 最近一次断言验证成功时间。
	LastUsedAt *time.Time `json:"last_used_at"`
}

// TableName 显式钉表名（GORM 默认复数化对缩写词不稳定，注册表守卫也读字面量）。
func (WebAuthnCredential) TableName() string { return "webauthn_credentials" }
