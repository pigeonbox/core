package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRefreshToken_Blacklist 登出（黑名单）后的 token 不得再刷新出新 token。
// 此前 /api/v1/user/refresh 不校验黑名单，登出撤销可被绕过（设计文档 §9.6）。
func TestRefreshToken_Blacklist(t *testing.T) {
	SetJWTSecret("test-secret-for-refresh")
	t.Cleanup(func() { SetJWTSecret("") })

	ctx := context.Background()
	token, err := GenerateToken(7, "alice", "user")
	require.NoError(t, err)

	// 未吊销：可正常刷新
	newToken, err := RefreshToken(ctx, token)
	require.NoError(t, err)
	assert.NotEmpty(t, newToken)

	// 吊销后：刷新必须被拒（ErrTokenRevoked）
	RevokeToken(ctx, token, time.Minute)
	_, err = RefreshToken(ctx, token)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTokenRevoked), "期望 ErrTokenRevoked，实际: %v", err)

	// 新 token 不受旧 token 吊销影响
	_, err = ParseToken(newToken)
	require.NoError(t, err)
}

// TestRefreshToken_InheritsSessionID 刷新必须继承旧 sid（core#106）。
// 此前 RefreshToken 走 GenerateToken（sid 置空）⇒ 刷新一次会话回查即永久跳过，
// 「踢设备」止损被绕过。
// 负对照：把 GenerateTokenWithSession(..., claims.SessionID) 改回 GenerateToken
// ⇒ 本用例转红（新令牌 sid 变空）。
func TestRefreshToken_InheritsSessionID(t *testing.T) {
	SetJWTSecret("test-secret-for-refresh")
	t.Cleanup(func() { SetJWTSecret("") })

	token, err := GenerateTokenWithSession(7, "alice", "user", "sid-42")
	require.NoError(t, err)
	newToken, err := RefreshToken(context.Background(), token)
	require.NoError(t, err)

	claims, err := ParseToken(newToken)
	require.NoError(t, err)
	assert.Equal(t, "sid-42", claims.SessionID,
		"新令牌必须继承旧 sid（不得签出无 sid 令牌让会话回查永久跳过）")

	// 无 sid 的老令牌刷新后仍无 sid（不凭空造会话；其止损靠纪元判定）。
	// 身份参数与同包其它用例错开：claims 全同 + 同秒签发 ⇒ 两枚令牌字节串
	// 相同，前一用例写进全局黑名单的条目会命中这里（实测踩过）。
	oldStyle, err := GenerateToken(7007, "alice-nosid", "user")
	require.NoError(t, err)
	newStyle, err := RefreshToken(context.Background(), oldStyle)
	require.NoError(t, err)
	c2, err := ParseToken(newStyle)
	require.NoError(t, err)
	assert.Empty(t, c2.SessionID)
}

func TestRefreshToken_Invalid(t *testing.T) {
	SetJWTSecret("test-secret-for-refresh")
	t.Cleanup(func() { SetJWTSecret("") })

	_, err := RefreshToken(context.Background(), "not.a.jwt")
	require.Error(t, err)
}
