package middleware

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/oekit/pkg/cache"
	"github.com/oekit/pkg/ctxscope"
	"github.com/pigeonbox/contracts/errcode"
	"github.com/pigeonbox/core/pkg/auth"
)

// 请求级身份的 context 槽位（ctxscope.Key：命名空间 + 值类型双隔离）。
//
// 为什么换掉手搓的非导出 ctxKey 枚举：枚举只防**跨包键碰撞**，不钉值类型——
// ctx.Value(k).(uint) 用错类型时静默得到零值（认证信息「消失」而非报错）。
// ctxscope 的 Key[T] 把键与值类型绑在一起，且 From 对「绕过 With 直写」的值
// 一律按不存在处理，写入纪律由类型断言强制。零新增依赖（oekit/pkg 子包）。
var (
	ctxKeyUserID   = ctxscope.NewKey[uint]("pigeonbox/middleware.user_id")
	ctxKeyUsername = ctxscope.NewKey[string]("pigeonbox/middleware.username")
	ctxKeyRole     = ctxscope.NewKey[string]("pigeonbox/middleware.role")
	ctxKeyClientIP = ctxscope.NewKey[string]("pigeonbox/middleware.client_ip")
	ctxKeyAPIKeyID = ctxscope.NewKey[uint]("pigeonbox/middleware.api_key_id")
)

// 注入/读取工具：中间件将认证信息写入 ctx，service 层经此提取操作者（审计用）。
// apiKeyID 非 0 表示本次请求经用户级 API Key 认证（写入传输日志做 Key 粒度归因）。
func withIdentity(ctx context.Context, userID uint, username, role, ip string, apiKeyID uint) context.Context {
	ctx = ctxscope.With(ctx, ctxKeyUserID, userID)
	ctx = ctxscope.With(ctx, ctxKeyUsername, username)
	ctx = ctxscope.With(ctx, ctxKeyRole, role)
	ctx = ctxscope.With(ctx, ctxKeyClientIP, ip)
	if apiKeyID > 0 {
		ctx = ctxscope.With(ctx, ctxKeyAPIKeyID, apiKeyID)
	}
	return ctx
}

// APIKeyIDFromContext 从 ctx 读取认证所用的 API Key ID（JWT/匿名请求返回 false）。
func APIKeyIDFromContext(ctx context.Context) (uint, bool) {
	return ctxscope.From(ctx, ctxKeyAPIKeyID)
}

// UserIDFromContext 从 ctx 读取用户 ID（未认证返回 false）
func UserIDFromContext(ctx context.Context) (uint, bool) {
	return ctxscope.From(ctx, ctxKeyUserID)
}

// UsernameFromContext 从 ctx 读取用户名（未认证返回空串）
func UsernameFromContext(ctx context.Context) string {
	v, _ := ctxscope.From(ctx, ctxKeyUsername)
	return v
}

// RoleFromContext 从 ctx 读取角色
func RoleFromContext(ctx context.Context) string {
	v, _ := ctxscope.From(ctx, ctxKeyRole)
	return v
}

// ClientIPFromContext 从 ctx 读取已解析的客户端 IP
func ClientIPFromContext(ctx context.Context) string {
	v, _ := ctxscope.From(ctx, ctxKeyClientIP)
	return v
}

// SetJWTClaims JWT 身份 c.Set 四件套（键名单一真相源：user_id/username/role/auth_type，
// 与 transport 侧 ContextKey* 常量同字符串）。pkg 与 transport 两个中间件栈均委托此处。
func SetJWTClaims(c *app.RequestContext, claims *auth.Claims) {
	c.Set("user_id", claims.UserID)
	c.Set("username", claims.Username)
	c.Set("role", claims.Role)
	c.Set("auth_type", "jwt")
	// 5.2.0：当前会话标识（升级前签发的老令牌为空串）。
	// handler 侧「登录设备」列表据此标出「本机」，避免把自己列成可踢的设备。
	c.Set("session_id", claims.SessionID)
}

// SetIdentityHeaders 身份响应头三连（X-User-ID/X-Username/X-Role）。
func SetIdentityHeaders(c *app.RequestContext, claims *auth.Claims) {
	c.Header("X-User-ID", fmt.Sprintf("%d", claims.UserID))
	c.Header("X-Username", claims.Username)
	c.Header("X-Role", claims.Role)
}

// ===== 身份回查（2026-10-05 审计 P2：封禁/降权/改密即时生效）=====
//
// JWT 是无状态的：仅凭签名无法感知"签发后用户被 ban/降权/改密"。此前 API Key
// 每请求回查属主状态（apikey.go）而 JWT 不回查，两条认证路径不对称——封禁用户
// 的 JWT 可用到自然过期（默认 7 天），降权的 admin 同理，且 refresh 端点可无限
// 续期。注入 IdentityLoader 后，认证路径以 DB 当前值复核 status/role/会话纪元
// （users.session_epoch，变更点 +1）；短 TTL 缓存控制查询成本，变更方调
// InvalidateIdentity 可立即生效。

const (
	identityCacheTTL     = 30 * time.Second
	identityCacheMaxSize = 65536
)

// IdentityRecord 用户当前态快照（composition root 以 dao 桥注入）。
type IdentityRecord struct {
	Status string
	Role   string
	Epoch  int
	// Sessions 该用户**当前仍然有效**的会话 sid 集合（5.2.0）。
	// nil = 装配点未提供（跳过会话校验，只走纪元判定）；空 map = 该用户
	// 没有任何有效会话 ⇒ 带 sid 的令牌一律拒绝。
	//
	// 为什么不每请求查一次库：认证是最热的路径，会话校验搭 30s 身份缓存
	// 的便车；撤销会话时立刻 InvalidateIdentity ⇒ 生效延迟就是 0（不是 30s）。
	Sessions map[string]bool
}

var (
	// identityLoader 守卫锁：loader 只在 bootstrap/测试注入，与 GetOrLoad 的
	// Source 读取保持 race-clean（identityCache 本身的并发由 LocalCache 托管）
	identityLoaderMu sync.RWMutex
	identityLoader   func(ctx context.Context, userID uint) (*IdentityRecord, error)

	// identityCache 身份回查缓存。2026-10-18 起托管给 oekit/pkg/cache LocalCache
	// （TTL/容量驱逐/后台清扫均为其契约能力，替代手搓 map+惰性 GC 约 40 行）。
	// NullTTL=-1 保持历史语义：查无此人/loader 未就绪不缓存（恒回源）；
	// loader 错误不缓存（LocalCache 契约）。
	identityCache = newIdentityCache(nil)
)

// newIdentityCache 构造身份缓存；now 非 nil 时注入时钟（pkg v0.13.0 #9：
// 全部过期判定/写入时刻/清扫节奏均路由该时钟），供单测假时钟确定性驱动。
func newIdentityCache(now func() time.Time) *cache.LocalCache[IdentityRecord] {
	cfg := cache.LocalConfig[IdentityRecord]{
		TTL:     identityCacheTTL,
		MaxSize: identityCacheMaxSize,
		NullTTL: -1,
		Source: func(ctx context.Context, key string) (*IdentityRecord, error) {
			identityLoaderMu.RLock()
			fn := identityLoader
			identityLoaderMu.RUnlock()
			if fn == nil {
				return nil, nil
			}
			uid, err := strconv.ParseUint(key, 10, 64)
			if err != nil {
				return nil, nil
			}
			return fn(ctx, uint(uid))
		},
	}
	if now != nil {
		cfg.Now = now
	}
	return cache.NewLocalCache(cfg)
}

// identityCacheKey 用户 ID → 缓存键（LocalCache 键域为 string）
func identityCacheKey(userID uint) string {
	return strconv.FormatUint(uint64(userID), 10)
}

// SetIdentityLoader 注入身份加载器（bootstrap 调用一次；nil = 关闭回查，
// 退化为纯 JWT 语义——单测/轻量环境）。
func SetIdentityLoader(fn func(ctx context.Context, userID uint) (*IdentityRecord, error)) {
	identityLoaderMu.Lock()
	defer identityLoaderMu.Unlock()
	identityLoader = fn
}

// InvalidateIdentity 清除某用户的回查缓存（改密/封禁/降权落库后调用，
// 使本次变更绕过 TTL 立即生效）。
func InvalidateIdentity(userID uint) {
	_ = identityCache.Del(context.Background(), identityCacheKey(userID))
}

// identityOf 带缓存的回查。loader 未注入返回 (nil, nil)，调用方跳过复核。
func identityOf(ctx context.Context, userID uint) (*IdentityRecord, error) {
	rec, err := identityCache.GetOrLoad(ctx, identityCacheKey(userID))
	if err != nil || rec == nil {
		return nil, err
	}
	return rec, nil
}

// identityFresh 认证附加复核：用户 active + 会话纪元与 claim 一致。
// 通过时用 DB 角色回填 claim（防降权后旧 claim 仍带 admin）。
// 返回 false 表示应拒绝（强制路径 401 / 可选路径降级匿名）。
func identityFresh(ctx context.Context, claims *auth.Claims) bool {
	rec, err := identityOf(ctx, claims.UserID)
	if rec == nil {
		return err == nil // loader 未注入 → 维持纯 JWT 语义
	}
	if err != nil {
		return false // 查询失败 fail-closed（与 API Key 回查语义一致）
	}
	if rec.Status != "active" || rec.Epoch != claims.Epoch {
		return false
	}
	// 会话回查（5.2.0）：带 sid 的令牌必须命中「仍然有效」的会话行。
	// 无 sid（升级前签发）跳过 —— 由纪元/登出黑名单兜底，避免升级即全员下线。
	if claims.SessionID != "" && rec.Sessions != nil && !rec.Sessions[claims.SessionID] {
		return false
	}
	claims.Role = rec.Role
	return true
}

// IdentityFresh 导出的身份复核入口：AuthMiddleware 之外的认证路径
// （如 transport 层 UserAuth）共享同一 30s 缓存回查——封禁/降权/改密后
// 旧 token 不再在这些路由越界可用（2026-10-08 加固）。
func IdentityFresh(ctx context.Context, claims *auth.Claims) bool {
	return identityFresh(ctx, claims)
}

// respondJWTRejection 执行点拒绝型 → HTTP 映射（状态码/业务码/文案按各入口
// 既有口径；staleMsg 为身份回查文案——AuthMiddleware/AdminMiddleware 与
// UserAuth 有意不同，由调用方传入）。唯一 403 型拒绝是 CSRF。
func respondJWTRejection(c *app.RequestContext, rej JWTRejection, staleMsg string) {
	if rej == JWTCSRFRejected {
		c.JSON(http.StatusForbidden, map[string]interface{}{
			"code":    errcode.CodeForbidden,
			"message": "缺少 CSRF 头（Cookie 认证的写请求须携带 X-Requested-With: XMLHttpRequest）",
		})
		c.Abort()
		return
	}
	msg := RejectionMessage(rej)
	if rej == JWTIdentityStale {
		msg = staleMsg
	}
	c.JSON(http.StatusUnauthorized, map[string]interface{}{
		"code":    errcode.CodeUnauthorized,
		"message": msg,
	})
	c.Abort()
}

// injectJWTIdentity JWT 身份注入三连（c.Set 四件套 + ctx 身份 + 回显头）：
// RequestContext 与 ctx 双写——c.Get 供 handler 用，ctx value 供 service 层
// 提取审计操作者。pkg 侧三个 JWT 中间件共用同一方言（UserAuth 只做 c.Set，
// 方言差异见 transport/http/middleware/user_auth.go 注释）。
// 回归备注（2026-10-08）：此前可选路径只写 c.Set ⇒ JWT 用户被
// UserIDFromContext 静默当匿名（直传闸门误判、传输日志归因恒空、自定义取件码
// 失效）——双写缺一不可。
func injectJWTIdentity(ctx context.Context, c *app.RequestContext, claims *auth.Claims) context.Context {
	SetJWTClaims(c, claims)
	ctx = withIdentity(ctx, claims.UserID, claims.Username, claims.Role, ClientIP(c), 0)
	SetIdentityHeaders(c, claims)
	return ctx
}

// AuthMiddleware JWT认证中间件
//
// 令牌来源：Authorization: Bearer 头或会话 Cookie（pb_token，浏览器端默认；
// Cookie 认证的非安全方法另有 CSRF 头门禁，见 session.go）。
// 判定链经 EvaluateJWT 单一执行点（core#173 第四片收口），本函数只做
// 拒绝映射与身份注入。
func AuthMiddleware() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		token, viaCookie := SessionToken(c)
		claims, rej := EvaluateJWT(ctx, c, token, viaCookie, PolicySession)
		if rej != JWTAccepted {
			respondJWTRejection(c, rej, "账号状态已变更，请重新登录")
			return
		}
		c.Next(injectJWTIdentity(ctx, c, claims))
	}
}

// AdminMiddleware 管理员权限中间件
//
// 实现（2026-10-05 审计 P0 修复）：认证+授权必须在本中间件内联完成后统一放行。
// 此前复用 AuthMiddleware()——其成功路径会 c.Next 把链上剩余 handler（含业务
// handler）先执行完，返回后才做角色检查：写操作已生效、读操作的响应体已生成
// （hertz 的 JSON 写入是追加语义，403 只会拼在业务响应后面）。任意注册用户
// 因此可执行全部 /admin/* 与 /api/v1/mcp。
// 判定链经 EvaluateJWT 单一执行点（core#173 第四片收口）；角色关在放行前独立判定。
func AdminMiddleware() app.HandlerFunc {
	// 不需要认证的路径白名单
	skipPaths := map[string]bool{
		"/admin/login": true,
	}

	return func(ctx context.Context, c *app.RequestContext) {
		// 检查是否在白名单中
		if skipPaths[string(c.URI().Path())] {
			c.Next(ctx)
			return
		}

		// 认证（令牌来源：Bearer 头或会话 Cookie）+ 授权内联完成
		token, viaCookie := SessionToken(c)
		claims, rej := EvaluateJWT(ctx, c, token, viaCookie, PolicySession)
		if rej != JWTAccepted {
			respondJWTRejection(c, rej, "账号状态已变更，请重新登录")
			return
		}

		// 角色检查先于任何放行（身份回查已把 DB 角色回填 claim，防降权旧 claim）
		if claims.Role != "admin" {
			c.JSON(http.StatusForbidden, map[string]interface{}{
				"code":    errcode.CodeForbidden,
				"message": "Admin access required",
			})
			c.Abort()
			return
		}

		c.Next(injectJWTIdentity(ctx, c, claims))
	}
}

// OptionalAuthMiddleware 可选认证中间件（不强制要求登录）
//
// 判定链经 EvaluateJWT 单一执行点（PolicyOptional：吊销恒查、不做 CSRF 门禁
// ——身份在此仅影响配额归属/审计归因、非授权边界；半会话不在此拒——与强制
// 面有意不对称，pending-2FA 令牌按普通身份注入）。
// 任一拒绝型一律降级匿名。
func OptionalAuthMiddleware() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		token, _ := SessionToken(c)
		claims, rej := EvaluateJWT(ctx, c, token, false, PolicyOptional)
		if rej != JWTAccepted {
			c.Next(ctx)
			return
		}
		c.Next(injectJWTIdentity(ctx, c, claims))
	}
}
