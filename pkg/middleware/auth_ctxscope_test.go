package middleware

import (
	"context"
	"testing"
)

// 身份槽位收编 ctxscope 后多出的一条硬保证：**身份只能经 withIdentity 写入**。
// 这条用例锁住它——绕过单写入口直写同名槽位不会被当成认证身份（旧实现里
// 「键相同 + 类型相同」的直写会被照单全收）。
func TestIdentityContextKeys_BypassWriteNotTrusted(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxKeyUserID, uint(99))
	if uid, ok := UserIDFromContext(ctx); ok || uid != 0 {
		t.Fatalf("绕过 withIdentity 的直写被采信：uid=%d ok=%v", uid, ok)
	}

	// 反向对照：经 withIdentity 写入的必须读得回（否则判据只是「什么都读不到」）。
	okCtx := withIdentity(context.Background(), 42, "alice", "admin", "10.0.0.1", 7)
	if uid, ok := UserIDFromContext(okCtx); !ok || uid != 42 {
		t.Fatalf("正常写入读不回：uid=%d ok=%v", uid, ok)
	}
	if got := UsernameFromContext(okCtx); got != "alice" {
		t.Fatalf("username 读回不一致：%q", got)
	}
	if got := RoleFromContext(okCtx); got != "admin" {
		t.Fatalf("role 读回不一致：%q", got)
	}
	if got := ClientIPFromContext(okCtx); got != "10.0.0.1" {
		t.Fatalf("client ip 读回不一致：%q", got)
	}
	if kid, ok := APIKeyIDFromContext(okCtx); !ok || kid != 7 {
		t.Fatalf("api key id 读回不一致：%d %v", kid, ok)
	}

	// JWT 请求（apiKeyID=0）不写该槽位 ⇒ 读侧 false（与收编前同口径）。
	jwtCtx := withIdentity(context.Background(), 42, "alice", "admin", "10.0.0.1", 0)
	if kid, ok := APIKeyIDFromContext(jwtCtx); ok || kid != 0 {
		t.Fatalf("无 API Key 的请求读出了 Key ID：%d %v", kid, ok)
	}
}
