package auth

// L8 · auth_bypass：半会话（pending-2FA）与管理员令牌的两条旁路防线。
//
// 威胁 1（半会话续签旁路）：OIDC 回调对「admin 且开了 TOTP」的用户不签发正式
// 会话，只发半会话（MFAPending=true）。若半会话能被 RefreshToken 续签，
// 攻击者只要拿到半会话（尚未完成二验）就能洗成正式会话 ⇒ 二形验证被绕过。
// pkg/auth.RefreshToken 有 ErrMFAPending 分支，但此前**没有任何用例**走到它。
//
// 威胁 2（管理员令牌）：GenerateAdminToken 此前 0 覆盖——角色声明写错
// （比如写成 "Admin"）会让所有管理员令牌静默降级或失效，且无测试会红。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestL8_PendingMFATokenIsMarked 半会话必须带 MFAPending 标记且保留身份声明。
func TestL8_PendingMFATokenIsMarked(t *testing.T) {
	l8SetSecret(t)

	tok, err := GeneratePendingMFAToken(3, "alice", "admin")
	require.NoError(t, err)
	require.NotEmpty(t, tok)

	claims, err := ParseToken(tok)
	require.NoError(t, err)
	require.NotNil(t, claims)
	assert.True(t, claims.MFAPending, "半会话必须带 MFAPending 标记（认证面据此拒绝）")
	assert.Equal(t, uint(3), claims.UserID)
	assert.Equal(t, "alice", claims.Username)
	assert.Equal(t, "admin", claims.Role)
}

// TestL8_PendingMFATokenCannotBeRefreshed 半会话不得续签（二验旁路的核心判据）。
//
// 负对照：删掉 RefreshToken 里的 claims.MFAPending 分支 ⇒ 本用例转红
// （半会话被洗成正式会话）。
func TestL8_PendingMFATokenCannotBeRefreshed(t *testing.T) {
	l8SetSecret(t)

	pending, err := GeneratePendingMFAToken(3, "alice", "admin")
	require.NoError(t, err)

	refreshed, err := RefreshToken(context.Background(), pending)
	require.Error(t, err, "半会话必须拒绝续签")
	assert.ErrorIs(t, err, ErrMFAPending)
	assert.Empty(t, refreshed, "半会话续签不得产出新令牌")
}

// TestL8_AdminTokenCarriesAdminRole 管理员令牌的角色声明必须是精确的 "admin"。
//
// 角色字面量的唯一判据点是 pkg/authz.IsAdminRole（role == "admin"）——
// 写成 "Admin"/"ADMIN" 会被判成非管理员（全站管理员瞬间失权且无测试报警）。
func TestL8_AdminTokenCarriesAdminRole(t *testing.T) {
	l8SetSecret(t)

	tok, err := GenerateAdminToken(9, "root")
	require.NoError(t, err)
	require.NotEmpty(t, tok)

	claims, err := ParseToken(tok)
	require.NoError(t, err)
	require.NotNil(t, claims)
	assert.Equal(t, "admin", claims.Role)
	assert.Equal(t, uint(9), claims.UserID)
	assert.Equal(t, "root", claims.Username)
	assert.False(t, claims.MFAPending, "管理员正式令牌不得带半会话标记")
}

// TestL8_AdminTokenRejectedByForeignSecret 管理员令牌同样受签名保护（换密钥即失效）。
func TestL8_AdminTokenRejectedByForeignSecret(t *testing.T) {
	l8SetSecret(t)

	tok, err := GenerateAdminToken(9, "root")
	require.NoError(t, err)

	SetJWTSecret("another-secret-after-issuance-32chars-long!!")
	t.Cleanup(func() { SetJWTSecret("") })

	claims, err := ParseToken(tok)
	require.Error(t, err, "密钥更换后旧令牌必须失效")
	assert.ErrorIs(t, err, ErrInvalidToken)
	assert.Nil(t, claims)
}
