// core#119 同族收口：OIDC JIT 建号按 IdP email claim **零校验**落库——
// 该值随后流入 SMTP To 头（notify.buildMessage），CRLF 注入类 claim
// （x@y.com\r\nBcc:evil）可外带通知正文（含取件码）。收口走注册面同款
// utils.ValidateEmail：非法 ⇒ 显式失败（ErrEmailInvalid 拒绝登录），
// 绝不静默建号/静默降级占位邮箱。
//
// 判据：verified+非法 ⇒ ErrEmailInvalid 且零建号；display-name 形态同拒；
// 未verified+非法 ⇒ 占位邮箱（raw 不落库，历史边界不动）。
//
// 负对照（已实跑）：删掉 matchOrCreate 的 ValidateEmail 闸 ⇒ 转红
// （注入 claim 被建号落库）。
package oidc

import (
	"context"
	"testing"
	"time"

	"github.com/oekit/pkg/secretx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/pkg/auth"
	"github.com/pigeonbox/core/repo/db/model"
)

func TestOIDC_MalformedEmailClaimRejected(t *testing.T) {
	for _, tc := range []struct {
		name  string
		email string
	}{
		{"CRLF 注入", "x@y.com\r\nBcc:evil@evil.com"},
		{"display-name 形态", "Name <x@y.com>"},
		{"纯垃圾", "not-an-email"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oidcDB := newOIDCDB(t)
			auth.SetJWTSecret("oidc-test-secret-0123456789abcdef")
			srv, _ := fakeIdPFull(t, tc.email, true)
			svc := NewService(oidcDB, Config{Enabled: true, Issuer: srv.URL,
				ClientID: "c", ClientSecret: secretx.NewSecureString("s")})

			_, err := svc.ExchangeCallback(context.Background(), "http://pb.local",
				"the-code", svc.signState(time.Now().Unix(), "n"), "n")
			require.ErrorIs(t, err, ErrEmailInvalid,
				"非法 email claim 必须显式拒绝（JIT 落库会进 SMTP To 头）")

			var cnt int64
			oidcDB.Model(&model.User{}).Count(&cnt)
			assert.Equal(t, int64(0), cnt, "非法邮箱不得建号落库")
		})
	}
}

// TestOIDC_MalformedEmailClaimUnverifiedPlaceholder 未验证邮箱不受闸（历史
// 边界不动）：不落 raw claim（占位邮箱），但也**不**因格式非法拒绝登录——
// 只有会落库/参与匹配的 verified 邮箱才过严格校验。
func TestOIDC_MalformedEmailClaimUnverifiedPlaceholder(t *testing.T) {
	oidcDB := newOIDCDB(t)
	auth.SetJWTSecret("oidc-test-secret-0123456789abcdef")
	srv, _ := fakeIdPFull(t, "x@y.com\r\nBcc:evil@evil.com", false)
	svc := NewService(oidcDB, Config{Enabled: true, Issuer: srv.URL,
		ClientID: "c", ClientSecret: secretx.NewSecureString("s")})

	token, err := svc.ExchangeCallback(context.Background(), "http://pb.local",
		"the-code", svc.signState(time.Now().Unix(), "n"), "n")
	require.NoError(t, err, "未验证邮箱不落库 ⇒ 不在收口闸范围（历史行为不动）")
	require.NotEmpty(t, token)

	var u model.User
	require.NoError(t, oidcDB.Where("oidc_sub = ?", "user-sub-42").First(&u).Error)
	assert.NotContains(t, u.Email, "Bcc:evil", "raw claim 绝不落库（走占位邮箱）")
	assert.Contains(t, u.Email, "@oidc.local", "未验证邮箱应落占位邮箱")
}
