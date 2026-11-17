package auth

// L8 · auth_bypass：JWT 签名算法白名单（alg=none / 非 HS256 一律拒绝）。
//
// 威胁：ParseToken 若不限定签名算法，攻击者可用 alg=none（无签名）或换用同一密钥
// 的 HS512/HS384 伪造任意身份（含 admin）⇒ 认证旁路。JWT 最经典的伪造向量。
//
// 判据（三条都要成立）：
//  1. alg=none 的令牌解析失败；
//  2. **同一密钥**签出的 HS512/HS384 令牌同样解析失败——证明拒绝来自算法白名单，
//     而不是「签名对不上」（否则「密钥已泄漏」的等价路径仍可伪造身份）；
//  3. HS256 + 正确密钥的令牌照常通过（判据没把正常路径一起毙掉）。
//
// 负对照：删掉 ParseToken 里的 jwt.WithValidMethods([]string{"HS256"}) ⇒ 用例②
// 转红（同一密钥的 HS512 令牌被当作有效身份放行）。

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// l8Secret 本文件统一的测试密钥（够长，避免被任何"弱密钥"启发式误伤）。
const l8Secret = "l8-test-secret-for-alg-whitelist-32chars!"

// l8SetSecret 注入测试密钥并在用例结束后清空（全局密钥面必须复位）。
func l8SetSecret(t *testing.T) {
	t.Helper()
	SetJWTSecret(l8Secret)
	t.Cleanup(func() { SetJWTSecret("") })
}

// l8Claims 构造一份攻击者视角的声明（role=admin ⇒ 放行即等于管理权限）。
func l8Claims() *Claims {
	return &Claims{
		UserID:   1,
		Username: "attacker",
		Role:     "admin",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "PigeonBox",
		},
	}
}

// TestL8_JWTRejectsNoneAlgorithm alg=none（无签名）必须解析失败。
func TestL8_JWTRejectsNoneAlgorithm(t *testing.T) {
	l8SetSecret(t)

	tok, err := jwt.NewWithClaims(jwt.SigningMethodNone, l8Claims()).
		SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)
	require.Contains(t, tok, "eyJ") // 形如 header.payload. （签名段为空）

	claims, err := ParseToken(tok)
	require.Error(t, err, "alg=none 令牌必须被拒（无签名=无身份）")
	assert.ErrorIs(t, err, ErrInvalidToken)
	assert.Nil(t, claims)
}

// TestL8_JWTRejectsNonHS256Algorithm 同一密钥签出的 HS512/HS384 令牌必须解析失败。
//
// 关键在「同一密钥」：签名本身是有效的，拒绝只能来自算法白名单。
func TestL8_JWTRejectsNonHS256Algorithm(t *testing.T) {
	l8SetSecret(t)

	for _, tc := range []struct {
		name   string
		method jwt.SigningMethod
	}{
		{"HS512", jwt.SigningMethodHS512},
		{"HS384", jwt.SigningMethodHS384},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok, err := jwt.NewWithClaims(tc.method, l8Claims()).SignedString([]byte(l8Secret))
			require.NoError(t, err)

			claims, err := ParseToken(tok)
			require.Error(t, err, "%s 不在算法白名单 ⇒ 必须拒绝（同一密钥签出也拒）", tc.name)
			assert.ErrorIs(t, err, ErrInvalidToken)
			assert.Nil(t, claims)
		})
	}
}

// TestL8_JWTValidHS256StillAccepted 对照组：白名单内的 HS256 + 正确密钥照常放行。
//
// 没有这条，判据可能因为「一律拒绝」而恒绿（假绿）。
func TestL8_JWTValidHS256StillAccepted(t *testing.T) {
	l8SetSecret(t)

	tok, err := GenerateToken(7, "alice", "user")
	require.NoError(t, err)

	claims, err := ParseToken(tok)
	require.NoError(t, err)
	require.NotNil(t, claims)
	assert.Equal(t, uint(7), claims.UserID)
	assert.Equal(t, "alice", claims.Username)
	assert.Equal(t, "user", claims.Role)
}

// TestL8_JWTRejectsForeignSecret 另一把密钥签出的 HS256 令牌必须解析失败。
func TestL8_JWTRejectsForeignSecret(t *testing.T) {
	l8SetSecret(t)

	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, l8Claims()).
		SignedString([]byte("a-completely-different-secret-32chars"))
	require.NoError(t, err)

	claims, err := ParseToken(tok)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidToken)
	assert.Nil(t, claims)
}
