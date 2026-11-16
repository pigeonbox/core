package middleware

import (
	"context"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/test/assert"
	"github.com/pigeonbox/core/pkg/auth"
)

// 执行点契约（core#173 第四片：Authentication 收口）。
// 中间件测试（auth_test/apikey_test/user_auth_test）钉的是 HTTP 映射，
// 这里直接钉判定核本身：拒绝分型、策略位、以及「可选面不拒半会话」这一
// 与强制面的有意不对称（Policy 开关删掉 ⇒ 此处转红）。

func TestEvaluateJWTRejectionKinds(t *testing.T) {
	auth.SetJWTSecret("test-secret-for-authn-evaluate-32chars")
	useFreshIdentityCache(t, nil)
	t.Cleanup(func() { auth.SetJWTSecret("") })

	normal, err := auth.GenerateToken(7, "bob", "user")
	if err != nil {
		t.Fatal(err)
	}
	pending, err := auth.GeneratePendingMFAToken(7, "bob", "admin")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 缺失 / 无效：任意策略
	_, rej := EvaluateJWT(ctx, nil, "", false, PolicySession)
	assert.DeepEqual(t, JWTMissingToken, rej)
	_, rej = EvaluateJWT(ctx, nil, "not-a-jwt", false, PolicySession)
	assert.DeepEqual(t, JWTInvalidToken, rej)

	// 正常令牌：全链放行（未注入身份回查 loader ⇒ 纯 JWT 语义；
	// 非 Cookie 通道 ⇒ CSRF 恒过，不触 RequestContext）
	claims, rej := EvaluateJWT(ctx, nil, normal, false, PolicySession)
	assert.DeepEqual(t, JWTAccepted, rej)
	if claims == nil || claims.UserID != 7 {
		t.Fatalf("放行时 claims 应非 nil 且 UserID=7，实际 %+v", claims)
	}

	// 半会话：强制面拒 / 可选面放（历史不对称，由 Policy 位钉住）
	_, rej = EvaluateJWT(ctx, nil, pending, false, PolicySession)
	assert.DeepEqual(t, JWTHalfSession, rej)
	_, rej = EvaluateJWT(ctx, nil, pending, false, PolicyOptional)
	assert.DeepEqual(t, JWTAccepted, rej)
}

// TestAuthnNarrowEntries 三个 handler 窄入口：各自对应一处历史内联序列，
// 序列收进执行点后语义逐项不变。
func TestAuthnNarrowEntries(t *testing.T) {
	auth.SetJWTSecret("test-secret-for-authn-narrow-32chars")
	useFreshIdentityCache(t, nil)
	t.Cleanup(func() { auth.SetJWTSecret("") })
	ctx := context.Background()

	normal, err := auth.GenerateToken(9, "carol", "user")
	if err != nil {
		t.Fatal(err)
	}
	pending, err := auth.GeneratePendingMFAToken(9, "carol", "user")
	if err != nil {
		t.Fatal(err)
	}

	// ClaimsOfToken：纯解析（登出取剩余有效期 / 回跳判半会话）
	if _, err := ClaimsOfToken("garbage"); err == nil {
		t.Fatal("垃圾令牌应解析失败")
	}
	if c, err := ClaimsOfToken(normal); err != nil || c.UserID != 9 {
		t.Fatalf("正常令牌应解析成功: err=%v claims=%+v", err, c)
	}

	// FreshClaimsOfToken：ParseToken + 身份回查（refresh 续签前复核，core#106）
	if _, err := FreshClaimsOfToken(ctx, "garbage"); err == nil {
		t.Fatal("垃圾令牌应复核失败")
	}
	if c, err := FreshClaimsOfToken(ctx, normal); err != nil || c.UserID != 9 {
		t.Fatalf("正常令牌应复核通过: err=%v claims=%+v", err, c)
	}

	// HalfSessionClaims：必须是 pending-2FA 半会话且未吊销（MFA 换发半程）
	if _, err := HalfSessionClaims(ctx, normal); err == nil {
		t.Fatal("正式令牌不该被当作半会话放行")
	}
	if c, err := HalfSessionClaims(ctx, pending); err != nil || !c.MFAPending {
		t.Fatalf("半会话应通过: err=%v claims=%+v", err, c)
	}
}
