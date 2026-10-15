package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/pigeonbox/contracts/errcode"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/kit/async"
	"golang.org/x/time/rate"
)

// apiKeyPlainPrefix API Key 明文前缀（与 app/user 签发端一致）。
// 同时承担 Bearer 凭证与 JWT 的确定性区分：pb_sk_ 开头必为 Key（JWT 恒为 eyJ 开头）。
const apiKeyPlainPrefix = "pb_sk_"

// errInvalidAPIKey 统一拒绝原因（对外一律 401 "Invalid API Key"，不区分过期/吊销/封禁，防枚举）。
var errInvalidAPIKey = errors.New("invalid api key")

// errAPITokenDisabled 认证总开关关闭（security.api_token.enabled=false，紧急停用）。
var errAPITokenDisabled = errors.New("api token auth disabled")

// errPerKeyRateLimited 单 Key 独立限流触发（security.api_token.per_key_qps）。
var errPerKeyRateLimited = errors.New("api key rate limited")

// apiTokenEnabled API Key 认证总开关；无全局配置（单测等）默认开启。
func apiTokenEnabled() bool {
	if c := conf.GetGlobalConfig(); c != nil {
		return c.Security.APIToken.Enabled
	}
	return true
}

// extractAPIKey 从请求头提取 API Key。拒绝 query 传参（防访问日志/Referer/代理日志泄露）。
// 支持三种形式：
//  1. Authorization: Bearer pb_sk_xxx（首选）
//  2. Authorization: ApiKey xxx
//  3. X-API-Key: xxx
//
// 返回 (key, true) 表示请求显式携带 Key（无论是否有效，调用方须 fail-closed）；
// 返回 ("", false) 表示未携带 Key（按匿名/JWT 语义处理）。
func extractAPIKey(c *app.RequestContext) (string, bool) {
	authHeader := strings.TrimSpace(string(c.GetHeader("Authorization")))
	if authHeader != "" {
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) == 2 {
			token := strings.TrimSpace(parts[1])
			if token != "" && strings.EqualFold(parts[0], "ApiKey") {
				return token, true
			}
			if parts[0] == "Bearer" && strings.HasPrefix(token, apiKeyPlainPrefix) {
				return token, true
			}
		}
	}
	if key := strings.TrimSpace(string(c.GetHeader("X-API-Key"))); key != "" {
		return key, true
	}
	return "", false
}

// sha256Hex Key 摘要（与 app/user.HashAPIKey 同算法：hex(SHA-256)）
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ===== TouchLastUsed 写节流 =====

// touchInterval 每 Key 两次 last_used_at 落库的最小间隔。
// 多实例各自节流即可（目的仅是降写放大，非精确统计）。
const touchInterval = time.Minute

var touchThrottle sync.Map // keyID(uint) → last touch time.Time

func shouldTouchLastUsed(keyID uint, now time.Time) bool {
	if v, ok := touchThrottle.Load(keyID); ok && now.Sub(v.(time.Time)) < touchInterval {
		return false
	}
	touchThrottle.Store(keyID, now)
	return true
}

// ===== Per-Key 独立限流（波次3：防单把 Key 被盗后高速滥用）=====

type keyLimiterEntry struct {
	limiter    *rate.Limiter
	lastAccess time.Time
}

var perKeyLimiters sync.Map // keyID(uint) → *keyLimiterEntry
var perKeyGCOnce sync.Once

// perKeyQPS 读取单 Key 限流参数；无全局配置（单测）返回 0=不限。
func perKeyQPS() (int, int) {
	if c := conf.GetGlobalConfig(); c != nil {
		return c.Security.APIToken.PerKeyQPS, c.Security.APIToken.PerKeyBurst
	}
	return 0, 0
}

// allowPerKey 单 Key 令牌桶。进程内计数（与 Touch 节流同语义，多实例各自限），
// 吊销 Key 不再产生访问，后台协程周期清理 1h 未触达的条目。
func allowPerKey(keyID uint, qps, burst int) bool {
	if qps <= 0 {
		return true
	}
	if burst <= 0 {
		burst = 2 * qps
	}
	perKeyGCOnce.Do(func() {
		// 常驻 GC 无监督者，且 Range 内是无 ok 形式类型断言——GoSafe 兜底防
		// 断言/演进 panic 带崩认证路径所在进程。
		async.GoSafe(func() {
			for range time.Tick(10 * time.Minute) {
				now := time.Now()
				perKeyLimiters.Range(func(k, v any) bool {
					if now.Sub(v.(*keyLimiterEntry).lastAccess) > time.Hour {
						perKeyLimiters.Delete(k)
					}
					return true
				})
			}
		})
	})
	now := time.Now()
	v, _ := perKeyLimiters.LoadOrStore(keyID, &keyLimiterEntry{
		limiter: rate.NewLimiter(rate.Limit(qps), burst), lastAccess: now,
	})
	e := v.(*keyLimiterEntry)
	e.lastAccess = now
	return e.limiter.Allow()
}

// ===== 持久化能力（注入式：pkg 不依赖 repo/db/dao，composition root 装配）=====

// APIKeyPrincipal 认证所需的最小身份事实（user_api_keys × users 两表提炼，
// pkg 不直接引用 repo 模型类型）。
type APIKeyPrincipal struct {
	KeyID    uint
	UserID   uint
	Username string
	Role     string
}

// APIKeyStore API Key 认证所需持久化能力（bootstrap 注入 dao 桥实现）。
// 语义约定：Key 不存在/已吊销/已过期/属主非 active 状态统一返回 error，
// 调用方不区分原因（防枚举）。
type APIKeyStore interface {
	FindActiveByHash(ctx context.Context, keyHash string) (*APIKeyPrincipal, error)
	TouchLastUsed(ctx context.Context, keyID uint, ip string) error
}

var apiKeyStore APIKeyStore // nil = 未装配，validateAPIKey fail-closed

// SetAPIKeyStore 注入持久化实现（bootstrap 装配调用）。
func SetAPIKeyStore(s APIKeyStore) { apiKeyStore = s }

// ===== 校验与身份注入 =====

// validateAPIKey 校验明文 Key 并注入身份（与 JWT 中间件双写方言一致：
// c.Set 供 handler，ctx 注入供 service 层 UserIDFromContext 审计提取）。
// 防护链：lockout(apikey|IP 防爆破) → 摘要查库(吊销/过期) → 用户状态 → 触碰节流。
// 失败一律返回 errInvalidAPIKey 或 *LockedError，不泄露具体原因。
// 认证查询不走缓存：吊销/封禁必须即时生效（每请求 2 个索引查询，亚毫秒级）。
func validateAPIKey(ctx context.Context, c *app.RequestContext, plainKey string) (context.Context, error) {
	// 总开关：紧急停用时不做任何校验/计数，携带 Key 一律 401（fail-closed）
	if !apiTokenEnabled() {
		return ctx, errAPITokenDisabled
	}

	lock := GetDefaultLockout()
	lockKey := FormatLockKey("apikey", ClientIP(c))
	if remain, locked := lock.CheckLocked(ctx, lockKey); locked {
		return ctx, &LockedError{RemainingSeconds: remain}
	}

	if apiKeyStore == nil {
		// 持久化未装配（bootstrap 未桥接/单测未注入）：fail-closed，
		// 与无效 Key 同路径计入防爆破，不泄露装配缺失细节
		_, _ = lock.RecordFailure(ctx, lockKey)
		return ctx, errInvalidAPIKey
	}
	principal, err := apiKeyStore.FindActiveByHash(ctx, sha256Hex(plainKey))
	if err != nil {
		// Key 不存在/已吊销/已过期/属主非 active 统一按无效 Key 处理（防枚举）
		_, _ = lock.RecordFailure(ctx, lockKey)
		return ctx, errInvalidAPIKey
	}
	lock.Reset(ctx, lockKey)

	// 单 Key 独立限流（先于 Touch：被限流的请求不计入使用统计）
	if qps, burst := perKeyQPS(); !allowPerKey(principal.KeyID, qps, burst) {
		return ctx, errPerKeyRateLimited
	}

	if shouldTouchLastUsed(principal.KeyID, time.Now()) {
		_ = apiKeyStore.TouchLastUsed(ctx, principal.KeyID, ClientIP(c))
	}

	c.Set("user_id", principal.UserID)
	c.Set("username", principal.Username)
	c.Set("role", principal.Role)
	c.Set("api_key_id", principal.KeyID) // 与 transport 侧 ContextKeyAPIKeyID 同字符串
	c.Set("auth_type", "api_key")        // 与 transport 侧 ContextKeyAuthType 同字符串
	c.Header("X-User-ID", fmt.Sprintf("%d", principal.UserID))
	// 回显头消毒（2026-10-08 加固）：新用户名建号时已白名单校验，但 legacy
	// 存量用户名从未复验——header 值必须只含可见安全字符，防响应拆分/注入面
	c.Header("X-Username", sanitizeHeaderValue(principal.Username))
	c.Header("X-Role", principal.Role)

	return withIdentity(ctx, principal.UserID, principal.Username, principal.Role, ClientIP(c), principal.KeyID), nil
}

// sanitizeHeaderValue 回显头消毒：剥离控制字符（CR/LF/其他 C0）与空白，
// 只留可打印可见字符——存量 legacy 用户名未经过新白名单校验，出响应头前兜底。
func sanitizeHeaderValue(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r > 0x1f && r != 0x7f {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// respondAPIKeyError 统一错误响应：总开关关闭 → 401；被锁定/单 Key 限流 → 429；无效 → 401（统一文案防枚举）。
func respondAPIKeyError(c *app.RequestContext, err error) {
	if errors.Is(err, errAPITokenDisabled) {
		c.Abort()
		c.JSON(http.StatusUnauthorized, map[string]interface{}{
			"code":    errcode.CodeUnauthorized,
			"message": "API token authentication is disabled",
		})
		return
	}
	if errors.Is(err, errPerKeyRateLimited) {
		c.Abort()
		c.JSON(http.StatusTooManyRequests, map[string]interface{}{
			"code":    errcode.CodeTooManyAttempts,
			"message": "请求过于频繁（API Key 限流），请稍后重试",
		})
		return
	}
	var le *LockedError
	if errors.As(err, &le) {
		c.Abort()
		c.JSON(http.StatusTooManyRequests, map[string]interface{}{
			"code":    errcode.CodeTooManyAttempts,
			"message": fmt.Sprintf("尝试过于频繁，已临时锁定，请 %d 秒后重试", le.RemainingSeconds),
		})
		return
	}
	c.Abort()
	c.JSON(http.StatusUnauthorized, map[string]interface{}{
		"code":    errcode.CodeUnauthorized,
		"message": "Invalid API Key",
	})
}

// ===== 对外中间件 =====

// OptionalAPIKey 可选 API Key 认证（fail-closed：携带即校验，无效一律 401/429）
func OptionalAPIKey() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		key, present := extractAPIKey(c)
		if !present {
			c.Next(ctx)
			return
		}
		newCtx, err := validateAPIKey(ctx, c, key)
		if err != nil {
			respondAPIKeyError(c, err)
			return
		}
		c.Next(newCtx)
	}
}

// OptionalIdentity 可选身份认证组：JWT（既有可选语义）→ API Key（fail-closed）。
// 未携带凭证 → 匿名；携带 Key 但无效 → 401；JWT 无效 → 匿名（会话过期 UX，与 Key 有意不对称）。
// 供 gen 路由挂载点整体替换 OptionalAuthMiddleware()（挂载函数直接 return 本切片）。
func OptionalIdentity() []app.HandlerFunc {
	return []app.HandlerFunc{OptionalAuthMiddleware(), OptionalAPIKey()}
}

// AdminOrAdminKey 管理面自动化通道（2026-10-14 P3）：admin JWT（会话/Bearer）
// 或 **admin 用户**的 API Key（ApiKey 头 / Bearer pbk_* / X-API-Key）。
// 与 UserOrAPIKey 边界（本文件 §4.1「绝不挂 /admin」）的区分：本中间件的 Key
// 路径强制 admin 角色复核——非管理员的 Key 一律 403；唯一消费方是 pb CLI 的
// 直传自动化（/admin/p2p/* 组），不开普通 Key 进管理面的口子。
func AdminOrAdminKey() app.HandlerFunc {
	admin := AdminMiddleware()
	return func(ctx context.Context, c *app.RequestContext) {
		key, present := extractAPIKey(c)
		if !present {
			admin(ctx, c)
			return
		}
		newCtx, err := validateAPIKey(ctx, c, key)
		if err != nil {
			respondAPIKeyError(c, err)
			return
		}
		v, _ := c.Get("role")
		if role, _ := v.(string); role != "admin" {
			c.JSON(http.StatusForbidden, map[string]interface{}{
				"code":    errcode.CodeForbidden,
				"message": "Admin access required",
			})
			c.Abort()
			return
		}
		c.Next(newCtx)
	}
}

// UserOrAPIKey 登录凭证二选一：API Key 或 JWT（含黑名单检查，复用 AuthMiddleware 完整语义）。
// 供 /api/v1 自定义 REST 组替换 transport UserAuth，向第三方开放 Key 访问。
// 边界（设计文档 §4.1）：绝不挂 /admin；/user/api-keys 保持 JWT-only（Key 不能管 Key）。
func UserOrAPIKey() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		if key, present := extractAPIKey(c); present {
			newCtx, err := validateAPIKey(ctx, c, key)
			if err != nil {
				respondAPIKeyError(c, err)
				return
			}
			c.Next(newCtx)
			return
		}
		AuthMiddleware()(ctx, c)
	}
}
