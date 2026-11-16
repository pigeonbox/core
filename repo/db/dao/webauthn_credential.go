package dao

import (
	"context"
	"time"

	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm"
)

// WebAuthnCredentialRepository WebAuthn/passkey 凭证存取（core#160）。
type WebAuthnCredentialRepository struct {
	conn *gorm.DB // 构造注入（去全局化终态，见 doc.go）
}

// NewWebAuthnCredentialRepository 构造 repository；gormDB 必填（去全局化终态，无全局回退）。
func NewWebAuthnCredentialRepository(gormDB *gorm.DB) *WebAuthnCredentialRepository {
	return &WebAuthnCredentialRepository{conn: gormDB}
}

func (r *WebAuthnCredentialRepository) db() *gorm.DB {
	return r.conn
}

// Create 落库新凭证（CredentialID 唯一索引兜重复登记）。
func (r *WebAuthnCredentialRepository) Create(ctx context.Context, cred *model.WebAuthnCredential) error {
	return r.db().WithContext(ctx).Create(cred).Error
}

// ListByUser 某用户的全部凭证（按登记时间正序）。
func (r *WebAuthnCredentialRepository) ListByUser(ctx context.Context, userID uint) ([]*model.WebAuthnCredential, error) {
	var out []*model.WebAuthnCredential
	err := r.db().WithContext(ctx).Where("user_id = ?", userID).Order("id ASC").Find(&out).Error
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CountByUser 某用户的凭证数（登录闸判「是否已登记 passkey」用）。
func (r *WebAuthnCredentialRepository) CountByUser(ctx context.Context, userID uint) (int64, error) {
	var n int64
	err := r.db().WithContext(ctx).Model(&model.WebAuthnCredential{}).Where("user_id = ?", userID).Count(&n).Error
	return n, err
}

// GetByCredentialID 按凭证 ID 查（重复登记拦截）；未找到返回 gorm.ErrRecordNotFound。
func (r *WebAuthnCredentialRepository) GetByCredentialID(ctx context.Context, credentialID string) (*model.WebAuthnCredential, error) {
	var out model.WebAuthnCredential
	err := r.db().WithContext(ctx).Where("credential_id = ?", credentialID).First(&out).Error
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateAfterLogin 断言验证成功后回写签名计数与最近使用时间（库在 FinishLogin
// 内已做克隆判据：回滚时把计数清零，这里原样持久化该结果）。
func (r *WebAuthnCredentialRepository) UpdateAfterLogin(ctx context.Context, credentialID string, signCount uint32, usedAt time.Time) error {
	return r.db().WithContext(ctx).Model(&model.WebAuthnCredential{}).
		Where("credential_id = ?", credentialID).
		Updates(map[string]interface{}{
			"sign_count":   signCount,
			"last_used_at": &usedAt,
		}).Error
}
