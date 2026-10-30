package model

import (
	"encoding/json"
	"strings"
	"time"

	"gorm.io/gorm"
)

// UserAPIKey 表示用户生成的 API Key 元信息，真实密钥仅在创建时返回
// KeyHash 存储经过 SHA-256 处理后的摘要，Prefix 便于调试时区分不同密钥
// Revoked 标记密钥是否已被撤销
// ExpiresAt 可选过期时间，为空表示长期有效
// LastUsedAt/LastUsedIP 记录最近一次使用时间与来源 IP（泄露排查）
// ExpiryNotifiedAt 临期通知发送时间（去重）
// Name 为用户自定义的标识，方便区分多把密钥

type UserAPIKey struct {
	gorm.Model
	UserID     uint   `gorm:"index"`
	Name       string `gorm:"size:100"`
	Prefix     string `gorm:"size:16"`
	KeyHash    string `gorm:"size:64;uniqueIndex"`
	LastUsedAt *time.Time
	LastUsedIP string `gorm:"size:45"`
	ExpiresAt  *time.Time
	// ExpiryNotifiedAt 临期通知去重标记；NULL=未通知
	ExpiryNotifiedAt *time.Time
	RevokedAt        *time.Time
	Revoked          bool `gorm:"default:false"`
	// Scopes 权限范围（JSON 数组文本，如 ["admin","user"]）；**空串 = 全量**
	// （与 scopes 机制引入前完全一致 ⇒ 老 Key 无需重建）。
	Scopes string `gorm:"size:200"`
}

// EncodeUserAPIKeyScopes 把权限范围切片编码为列值（空 = 全量 ⇒ 落库空串）。
func EncodeUserAPIKeyScopes(scopes []string) string {
	if len(scopes) == 0 {
		return ""
	}
	b, err := json.Marshal(scopes)
	if err != nil {
		return ""
	}
	return string(b)
}

// ScopesList 解析列值 → 权限范围切片（空/非法 = 全量）。
func (k *UserAPIKey) ScopesList() []string {
	return ParseUserAPIKeyScopes(k.Scopes)
}

// ParseUserAPIKeyScopes 解析权限范围列值 → 切片。
// **解析不出合法值 ⇒ 返回空切片（= 全量）**：宁可宽松（老数据/损坏数据不锁死用户），
// 也不能因为一行脏数据把整把 Key 变成不可用。
func ParseUserAPIKeyScopes(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	clean := make([]string, 0, len(out))
	for _, v := range out {
		v = strings.TrimSpace(v)
		if v != "" {
			clean = append(clean, v)
		}
	}
	if len(clean) == 0 {
		return nil
	}
	return clean
}

// TableName 指定表名
func (UserAPIKey) TableName() string {
	return "user_api_keys"
}
