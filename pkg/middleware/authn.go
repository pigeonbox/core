// authn.go 认证决策的单一执行点（core#173 第四片：Authentication 收口）。
//
// ## 为什么有这个文件
//
// 同一条「JWT 认证判定链」此前在三处各写一份且顺序/覆盖面互有出入：
//
//   - pkg/middleware/auth.go 的 AuthMiddleware / AdminMiddleware / OptionalAuthMiddleware；
//   - transport/http/middleware/user_auth.go 的 UserAuth（/api/v1 用户面，文案与
//     注入方言有意不同，但检查项与 AuthMiddleware 完全同序）；
//   - handler 里的内联判定（session_ops 续签、OIDC 回跳判半会话、MFA 换发
//     校验半会话）——直接调 pkg/auth 的 ParseToken/IsTokenRevoked 原语，
//     新入口照抄即可绕过收口点，且没有任何测试会红。
//
// 2026-11-16 收口于此：
//
//   - EvaluateJWT：四个中间件共用的判定核（缺失 → 解析 → 半会话 → 吊销 →
//     CSRF → 身份回查，顺序固定）；**不注入、不写响应**——各入口的文案/注入
//     方言由调用方映射，这是「行为零变化」的关键（UserAuth 的
//     "Session is no longer valid" 与 AuthMiddleware 的「账号状态已变更」
//     等既有差异一字不动）。
//   - ClaimsOfToken / FreshClaimsOfToken / HalfSessionClaims：handler 层
//     三个窄入口，各自**逐字对应**一处历史内联序列（读声明 / 续签前复核 /
//     半会话校验），handler 不再直接摸 pkg/auth 原语。
//
// 守卫：bootstrap/authn_entry_guard_test.go —— 路由链上的鉴权口径必须登记到
// 执行点承载文件、handler 源码出现裸判定（auth.ParseToken 等）⇒ 测试红。
package middleware

import (
	"context"
	"errors"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/pigeonbox/core/pkg/auth"
)

// JWTRejection EvaluateJWT 的判定结果（拒绝原因分型，供调用方映射各自的
// 状态码/业务码/文案——执行点不产生 HTTP 响应）。
type JWTRejection int

const (
	// JWTAccepted 放行（claims 非 nil）。
	JWTAccepted JWTRejection = iota
	// JWTMissingToken 未携带令牌。
	JWTMissingToken
	// JWTInvalidToken 解析失败（签名/过期/格式）。
	JWTInvalidToken
	// JWTHalfSession pending-2FA 半会话（core#180）。
	JWTHalfSession
	// JWTRevoked 注销黑名单命中。
	JWTRevoked
	// JWTCSRFRejected Cookie 认证的写请求缺 CSRF 头（唯一 403 型拒绝）。
	JWTCSRFRejected
	// JWTIdentityStale 身份回查不过（封禁/降权/改密/会话纪元）。
	JWTIdentityStale
)

// JWTPolicy 执行点策略位：各入口的既有差异在此**显式声明**，而不是靠调用方
// 记住该跑哪几项。吊销检查无开关（全部 JWT 入口历史语义一致，恒开）。
type JWTPolicy struct {
	// RejectHalfSession pending-2FA 令牌一律拒（强制登录面 true；
	// 可选身份面 false——半会话在此仅按普通身份注入，与历史一致）。
	RejectHalfSession bool
	// EnforceCSRF Cookie 认证的非安全方法须带 X-Requested-With 头
	// （可选身份不做 CSRF 门禁：身份只影响配额归属/审计，非授权边界）。
	EnforceCSRF bool
	// RequireFresh 身份回查（封禁/降权/改密即时生效 + DB 角色回填）。
	RequireFresh bool
}

// PolicySession 强制登录面全链（AuthMiddleware / AdminMiddleware / UserAuth 同款）。
var PolicySession = JWTPolicy{RejectHalfSession: true, EnforceCSRF: true, RequireFresh: true}

// PolicyOptional 可选身份面（OptionalAuthMiddleware：吊销恒查、其余按需降级匿名）。
var PolicyOptional = JWTPolicy{RequireFresh: true}

// EvaluateJWT 认证决策单一执行点：按 policy 跑固定顺序的检查链，
// 返回 (claims, 判定)。**不注入上下文、不写 HTTP 响应、不 c.Abort**——
// 调用方按自身方言映射（见 auth.go 的 respondJWTRejection / UserAuth 的
// respondUnauthorized 分支）。
//
// 判定顺序（与四条历史链逐项对齐，勿调整）：
// 缺失 → 解析 → 半会话(policy) → 吊销(恒) → CSRF(policy) → 身份回查(policy)。
func EvaluateJWT(ctx context.Context, c *app.RequestContext, token string, viaCookie bool, policy JWTPolicy) (*auth.Claims, JWTRejection) {
	if token == "" {
		return nil, JWTMissingToken
	}
	claims, err := auth.ParseToken(token)
	if err != nil {
		return nil, JWTInvalidToken
	}
	if policy.RejectHalfSession && claims.MFAPending {
		return nil, JWTHalfSession
	}
	if auth.IsTokenRevoked(ctx, token) {
		return nil, JWTRevoked
	}
	if policy.EnforceCSRF && !CSRFAllowed(c, viaCookie) {
		return nil, JWTCSRFRejected
	}
	if policy.RequireFresh && !identityFresh(ctx, claims) {
		return nil, JWTIdentityStale
	}
	return claims, JWTAccepted
}

// errNotFresh 身份回查未过（FreshClaimsOfToken 的内部区分；调用方只按
// 「复核失败」处理，与历史 `perr != nil || !fresh` 同分支语义一致）。
var errNotFresh = errors.New("identity no longer fresh")

// errNotHalfSession 声明合法但不是 pending-2FA 半会话。
var errNotHalfSession = errors.New("not a pending-2FA half session")

// errHalfSessionRevoked 半会话已吊销（一次性，防重放再换）。
var errHalfSessionRevoked = errors.New("half session revoked")

// ClaimsOfToken 读取令牌声明（唯一允许 handler 层做的「解析」动作）。
// 用途是**数据读取**：登出取剩余有效期、回跳判半会话等——有效性之外的
// 判定（回查/吊销）不在这里做，需要的走 FreshClaimsOfToken/HalfSessionClaims。
func ClaimsOfToken(token string) (*auth.Claims, error) {
	return auth.ParseToken(token)
}

// FreshClaimsOfToken ParseToken + 身份回查合一（refresh 端点续签前复核，
// core#106 口径：改密/封禁/降权/退出所有设备不能被「旧令牌换新令牌」绕过）。
// 任一不过返回 error——调用方按自身 401 文案应答（历史两因同分支）。
func FreshClaimsOfToken(ctx context.Context, token string) (*auth.Claims, error) {
	claims, err := auth.ParseToken(token)
	if err != nil {
		return nil, err
	}
	if !identityFresh(ctx, claims) {
		return nil, errNotFresh
	}
	return claims, nil
}

// HalfSessionClaims 半会话（pending-2FA）校验：必须解析成功、是半会话、
// 且未吊销，否则返回 error。MFA 换发端点（app/oidc.ExchangeMFASession）
// 的半程判定，与历史内联三连（ParseToken → MFAPending → IsTokenRevoked）
// 逐项等价。
func HalfSessionClaims(ctx context.Context, token string) (*auth.Claims, error) {
	claims, err := auth.ParseToken(token)
	if err != nil {
		return nil, err
	}
	if !claims.MFAPending {
		return nil, errNotHalfSession
	}
	if auth.IsTokenRevoked(ctx, token) {
		return nil, errHalfSessionRevoked
	}
	return claims, nil
}

// RejectionMessage 拒绝型 → 通用文案（AuthMiddleware / UserAuth 共用的四型）。
// CSRF 与身份回查两型文案各入口有意不同（见调用方），不在此表。
func RejectionMessage(rej JWTRejection) string {
	switch rej {
	case JWTMissingToken:
		return "未认证（缺少 Bearer 头或会话 Cookie）"
	case JWTInvalidToken:
		return "Invalid or expired token"
	case JWTHalfSession:
		return "需要完成二次验证"
	case JWTRevoked:
		return "Token has been revoked"
	}
	return ""
}
