package middleware

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/auth"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
	"gorm.io/gorm"
)

// =====================================================================
// API Key 中间件单测。
// sqlite :memory: 多连接各见独立库，必须钉死单连接（upload-governance 教训）。
// =====================================================================

// daoAPIKeyStore 测试桥：持久化能力接 dao（生产由 bootstrap 注入同形实现）。
type daoAPIKeyStore struct{ gormDB *gorm.DB }

func (s daoAPIKeyStore) FindActiveByHash(ctx context.Context, hash string) (*APIKeyPrincipal, error) {
	key, err := dao.NewUserAPIKeyRepository(s.gormDB).GetActiveByHash(ctx, hash)
	if err != nil {
		return nil, err
	}
	user, err := dao.NewUserRepository(s.gormDB).GetByID(ctx, key.UserID)
	if err != nil || user.Status != "active" {
		// 封禁/停用用户的 Key 视为无效 Key（同生产桥语义）
		return nil, errInvalidAPIKey
	}
	return &APIKeyPrincipal{KeyID: key.ID, UserID: user.ID, Username: user.Username, Role: user.Role}, nil
}

func (s daoAPIKeyStore) TouchLastUsed(ctx context.Context, keyID uint, ip string) error {
	return dao.NewUserAPIKeyRepository(s.gormDB).TouchLastUsed(ctx, keyID, ip)
}

func newAPIKeyTestEnv(t *testing.T) *gorm.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.User{}, &model.UserAPIKey{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	SetAPIKeyStore(daoAPIKeyStore{gormDB: g})
	t.Cleanup(func() { SetAPIKeyStore(nil) })
	// 重置全局锁定器为纯内存实例 + 清空触碰节流/单Key限流表，测试间隔离（同包直取）
	InitDefaultLockout(nil)
	t.Cleanup(func() { InitDefaultLockout(nil) })
	touchThrottle = sync.Map{}
	perKeyLimiters = sync.Map{}
	return g
}

func newFixtureUser(t *testing.T, g *gorm.DB, status string) *model.User {
	t.Helper()
	u := &model.User{
		Username:     fmt.Sprintf("u_%d", time.Now().UnixNano()),
		Email:        fmt.Sprintf("u_%d@test.local", time.Now().UnixNano()),
		PasswordHash: "x",
		Status:       status,
		Role:         "user",
	}
	require.NoError(t, dao.NewUserRepository(g).Create(context.Background(), u))
	return u
}

func newFixtureKey(t *testing.T, g *gorm.DB, userID uint, mutate func(*model.UserAPIKey)) (string, *model.UserAPIKey) {
	t.Helper()
	plain := apiKeyPlainPrefix + fmt.Sprintf("%032x", time.Now().UnixNano())
	rec := &model.UserAPIKey{UserID: userID, Name: "test", Prefix: plain[:12], KeyHash: sha256Hex(plain)}
	if mutate != nil {
		mutate(rec)
	}
	require.NoError(t, dao.NewUserAPIKeyRepository(g).Create(context.Background(), rec))
	return plain, rec
}

// performProbe 起 hertz 测试引擎挂中间件链，返回 (状态码, 响应体, 注入的身份, handler 收到的 ctx)。
func performProbe(t *testing.T, mws []app.HandlerFunc, url string, headers ...ut.Header) (int, string, map[string]interface{}, context.Context) {
	t.Helper()
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	var captured context.Context
	var identity map[string]interface{}
	handler := func(ctx context.Context, c *app.RequestContext) {
		captured = ctx
		identity = map[string]interface{}{}
		for _, k := range []string{"user_id", "username", "role", "api_key_id", "auth_type"} {
			if v, ok := c.Get(k); ok {
				identity[k] = v
			}
		}
		c.JSON(http.StatusOK, map[string]string{"ok": "1"})
	}
	args := append([]app.HandlerFunc{}, mws...)
	args = append(args, handler)
	h.GET("/probe", args...)
	w := ut.PerformRequest(h.Engine, "GET", url, nil, headers...)
	return w.Code, w.Body.String(), identity, captured
}

func hdr(k, v string) ut.Header { return ut.Header{Key: k, Value: v} }

// --- 提取规则 ---

func TestOptionalAPIKey_NoKey_Anonymous(t *testing.T) {
	g := newAPIKeyTestEnv(t)
	u := newFixtureUser(t, g, "active")
	newFixtureKey(t, g, u.ID, nil)
	code, body, identity, _ := performProbe(t, []app.HandlerFunc{OptionalAPIKey()}, "/probe")
	assert.Equal(t, 200, code, body)
	assert.Empty(t, identity, "未携带 Key 必须匿名放行，不注入身份")
}

func TestOptionalAPIKey_QueryParamIgnored(t *testing.T) {
	g := newAPIKeyTestEnv(t)
	u := newFixtureUser(t, g, "active")
	plain, _ := newFixtureKey(t, g, u.ID, nil)
	// query 传 Key 必须被无视（防日志/Referer 泄露），按匿名处理
	code, body, identity, _ := performProbe(t, []app.HandlerFunc{OptionalAPIKey()}, "/probe?api_key="+plain)
	assert.Equal(t, 200, code, body)
	assert.Empty(t, identity)
}

func TestOptionalAPIKey_ValidKey_InjectsIdentity(t *testing.T) {
	g := newAPIKeyTestEnv(t)
	u := newFixtureUser(t, g, "active")
	plain, rec := newFixtureKey(t, g, u.ID, nil)

	code, body, identity, ctx := performProbe(t, []app.HandlerFunc{OptionalAPIKey()}, "/probe",
		hdr("X-API-Key", plain))
	assert.Equal(t, 200, code, body)
	assert.Equal(t, u.ID, identity["user_id"])
	assert.Equal(t, u.Username, identity["username"])
	assert.Equal(t, "user", identity["role"])
	assert.Equal(t, rec.ID, identity["api_key_id"])
	assert.Equal(t, "api_key", identity["auth_type"])
	// ctx 方言（service 层审计提取）
	gotID, ok := UserIDFromContext(ctx)
	assert.True(t, ok, "API Key 认证必须注入 ctx 身份供 service 层审计")
	assert.Equal(t, u.ID, gotID)
	// Key 粒度归因（transfer_logs 写入用）
	gotKeyID, ok := APIKeyIDFromContext(ctx)
	assert.True(t, ok, "API Key 认证必须注入 ctx Key 归因")
	assert.Equal(t, rec.ID, gotKeyID)
}

func TestOptionalAPIKey_BearerPbSkPrefix(t *testing.T) {
	g := newAPIKeyTestEnv(t)
	u := newFixtureUser(t, g, "active")
	plain, _ := newFixtureKey(t, g, u.ID, nil)
	code, body, identity, _ := performProbe(t, []app.HandlerFunc{OptionalAPIKey()}, "/probe",
		hdr("Authorization", "Bearer "+plain))
	assert.Equal(t, 200, code, body)
	assert.Equal(t, u.ID, identity["user_id"])
}

func TestOptionalAPIKey_ApiKeyScheme(t *testing.T) {
	g := newAPIKeyTestEnv(t)
	u := newFixtureUser(t, g, "active")
	plain, _ := newFixtureKey(t, g, u.ID, nil)
	code, body, identity, _ := performProbe(t, []app.HandlerFunc{OptionalAPIKey()}, "/probe",
		hdr("Authorization", "ApiKey "+plain))
	assert.Equal(t, 200, code, body)
	assert.Equal(t, u.ID, identity["user_id"])
}

// --- fail-closed：携带即校验 ---

func TestOptionalAPIKey_InvalidKey_401(t *testing.T) {
	g := newAPIKeyTestEnv(t)
	u := newFixtureUser(t, g, "active")
	newFixtureKey(t, g, u.ID, nil)
	code, body, identity, _ := performProbe(t, []app.HandlerFunc{OptionalAPIKey()}, "/probe",
		hdr("X-API-Key", apiKeyPlainPrefix+"wrongwrong"))
	assert.Equal(t, 401, code)
	assert.Empty(t, identity, "无效 Key 不得放行到 handler")
	assert.Contains(t, body, "Invalid API Key")
}

func TestOptionalAPIKey_ExpiredKey_401(t *testing.T) {
	g := newAPIKeyTestEnv(t)
	u := newFixtureUser(t, g, "active")
	plain, _ := newFixtureKey(t, g, u.ID, func(k *model.UserAPIKey) {
		past := time.Now().Add(-time.Hour)
		k.ExpiresAt = &past
	})
	code, _, _, _ := performProbe(t, []app.HandlerFunc{OptionalAPIKey()}, "/probe", hdr("X-API-Key", plain))
	assert.Equal(t, 401, code)
}

func TestOptionalAPIKey_RevokedKey_401(t *testing.T) {
	g := newAPIKeyTestEnv(t)
	u := newFixtureUser(t, g, "active")
	plain, _ := newFixtureKey(t, g, u.ID, func(k *model.UserAPIKey) { k.Revoked = true })
	code, _, _, _ := performProbe(t, []app.HandlerFunc{OptionalAPIKey()}, "/probe", hdr("X-API-Key", plain))
	assert.Equal(t, 401, code)
}

func TestOptionalAPIKey_BannedUser_401(t *testing.T) {
	g := newAPIKeyTestEnv(t)
	u := newFixtureUser(t, g, "banned")
	plain, _ := newFixtureKey(t, g, u.ID, nil)
	code, _, _, _ := performProbe(t, []app.HandlerFunc{OptionalAPIKey()}, "/probe", hdr("X-API-Key", plain))
	assert.Equal(t, 401, code, "封禁用户的 Key 必须失效")
}

// --- 总开关 ---

func TestOptionalAPIKey_DisabledSwitch_401(t *testing.T) {
	g := newAPIKeyTestEnv(t)
	conf.SetGlobalConfig(&conf.AppConfiguration{Security: conf.SecurityConfig{
		APIToken: conf.APITokenConfig{Enabled: false},
	}})
	t.Cleanup(func() { conf.SetGlobalConfig(nil) })
	u := newFixtureUser(t, g, "active")
	plain, _ := newFixtureKey(t, g, u.ID, nil)
	code, body, identity, _ := performProbe(t, []app.HandlerFunc{OptionalAPIKey()}, "/probe", hdr("X-API-Key", plain))
	assert.Equal(t, 401, code, "开关关闭后携带 Key 的请求必须 401")
	assert.Empty(t, identity)
	assert.Contains(t, body, "disabled")
}

// --- per-Key 独立限流 ---

func TestAllowPerKey_TokenBucket(t *testing.T) {
	assert.True(t, allowPerKey(1, 0, 0), "qps=0 视为不限")
	assert.True(t, allowPerKey(2, 1, 1), "首次消耗桶内令牌")
	assert.False(t, allowPerKey(2, 1, 1), "桶空即拒")
	time.Sleep(1100 * time.Millisecond)
	assert.True(t, allowPerKey(2, 1, 1), "1s 后补充令牌")
}

func TestOptionalAPIKey_PerKeyRateLimit_429(t *testing.T) {
	g := newAPIKeyTestEnv(t)
	conf.SetGlobalConfig(&conf.AppConfiguration{Security: conf.SecurityConfig{
		APIToken: conf.APITokenConfig{Enabled: true, PerKeyQPS: 1, PerKeyBurst: 1},
	}})
	t.Cleanup(func() { conf.SetGlobalConfig(nil) })
	u := newFixtureUser(t, g, "active")
	plain, _ := newFixtureKey(t, g, u.ID, nil)

	code1, _, _, _ := performProbe(t, []app.HandlerFunc{OptionalAPIKey()}, "/probe", hdr("X-API-Key", plain))
	assert.Equal(t, 200, code1, "首次放行")
	code2, body2, _, _ := performProbe(t, []app.HandlerFunc{OptionalAPIKey()}, "/probe", hdr("X-API-Key", plain))
	assert.Equal(t, 429, code2, body2)
	assert.Contains(t, body2, "限流")
	// 匿名请求不受该 Key 的限流影响
	code3, _, _, _ := performProbe(t, []app.HandlerFunc{OptionalAPIKey()}, "/probe")
	assert.Equal(t, 200, code3)
}

// --- lockout 防爆破 ---

func TestOptionalAPIKey_LockoutAfterMaxFailures(t *testing.T) {
	g := newAPIKeyTestEnv(t)
	u := newFixtureUser(t, g, "active")
	plain, _ := newFixtureKey(t, g, u.ID, nil)
	bad := apiKeyPlainPrefix + "notarealkey000"
	// 默认 MaxAttempts=10：前 10 次 401，第 11 次起 429（连有效 Key 也拦）
	for i := 0; i < 10; i++ {
		code, _, _, _ := performProbe(t, []app.HandlerFunc{OptionalAPIKey()}, "/probe", hdr("X-API-Key", bad))
		assert.Equal(t, 401, code, "第 %d 次失败应仍为 401", i+1)
	}
	code, body, _, _ := performProbe(t, []app.HandlerFunc{OptionalAPIKey()}, "/probe", hdr("X-API-Key", bad))
	assert.Equal(t, 429, code, body)
	assert.Contains(t, body, "锁定")
	// 锁定期间有效 Key 也被拦（CheckLocked 前置）
	code, _, _, _ = performProbe(t, []app.HandlerFunc{OptionalAPIKey()}, "/probe", hdr("X-API-Key", plain))
	assert.Equal(t, 429, code)
}

// --- TouchLastUsed 节流 ---

func TestShouldTouchLastUsed_Throttle(t *testing.T) {
	now := time.Now()
	assert.True(t, shouldTouchLastUsed(1, now), "首次必须写")
	assert.False(t, shouldTouchLastUsed(1, now.Add(30*time.Second)), "60s 内不重复写")
	assert.True(t, shouldTouchLastUsed(1, now.Add(61*time.Second)), "超 60s 再写")
	assert.True(t, shouldTouchLastUsed(2, now), "不同 Key 互不影响")
}

// 守卫测试：认证注册表清扫。touchThrottle 此前无任何清理路径（每把 Key 一条
// 永存，含已吊销 Key）；现与 perKeyLimiters 共用 janitor，超龄条目清理行为中性
// ——throttle 条目超 touchInterval 本就放行重写，limiter 条目 1h 未触达即无流量。
func TestSweepAuthRegistries(t *testing.T) {
	now := time.Now()
	touchThrottle.Store(9001, now.Add(-2*time.Minute))
	touchThrottle.Store(9002, now.Add(-30*time.Second))
	perKeyLimiters.Store(9003, &keyLimiterEntry{limiter: rate.NewLimiter(1, 1), lastAccess: now.Add(-2 * time.Hour)})
	perKeyLimiters.Store(9004, &keyLimiterEntry{limiter: rate.NewLimiter(1, 1), lastAccess: now})

	sweepAuthRegistries(now)

	_, staleTouch := touchThrottle.Load(9001)
	_, freshTouch := touchThrottle.Load(9002)
	assert.False(t, staleTouch, "超 touchInterval 的 throttle 条目应被清")
	assert.True(t, freshTouch, "新鲜 throttle 条目保留")
	_, staleLimiter := perKeyLimiters.Load(9003)
	_, freshLimiter := perKeyLimiters.Load(9004)
	assert.False(t, staleLimiter, "1h 未触达的 limiter 条目应被清")
	assert.True(t, freshLimiter, "新鲜 limiter 条目保留")

	touchThrottle.Delete(9002)
	perKeyLimiters.Delete(9004)
}

func TestOptionalAPIKey_TouchLastUsedThrottled(t *testing.T) {
	g := newAPIKeyTestEnv(t)
	u := newFixtureUser(t, g, "active")
	plain, rec := newFixtureKey(t, g, u.ID, nil)
	repo := dao.NewUserAPIKeyRepository(g)
	ctx := context.Background()

	_, _, _, _ = performProbe(t, []app.HandlerFunc{OptionalAPIKey()}, "/probe", hdr("X-API-Key", plain))
	after1, err := repo.GetByID(ctx, rec.ID)
	require.NoError(t, err)
	require.NotNil(t, after1.LastUsedAt, "首次使用必须写 last_used_at")

	_, _, _, _ = performProbe(t, []app.HandlerFunc{OptionalAPIKey()}, "/probe", hdr("X-API-Key", plain))
	after2, err := repo.GetByID(ctx, rec.ID)
	require.NoError(t, err)
	assert.Equal(t, after1.LastUsedAt.Unix(), after2.LastUsedAt.Unix(), "60s 内第二次使用不得再写库")
}

// --- UserOrAPIKey ---

func TestUserOrAPIKey_ValidJWT(t *testing.T) {
	g := newAPIKeyTestEnv(t)
	auth.SetJWTSecret("test-secret-for-apikey")
	t.Cleanup(func() { auth.SetJWTSecret("") })
	u := newFixtureUser(t, g, "active")
	token, err := auth.GenerateToken(u.ID, u.Username, u.Role)
	require.NoError(t, err)
	code, body, identity, captured := performProbe(t, []app.HandlerFunc{UserOrAPIKey()}, "/probe",
		hdr("Authorization", "Bearer "+token))
	assert.Equal(t, 200, code, body)
	assert.Equal(t, u.ID, identity["user_id"])
	// JWT 路径不得带 Key 归因
	_, hasKeyID := APIKeyIDFromContext(captured)
	assert.False(t, hasKeyID, "JWT 认证不应有 API Key 归因")
}

func TestUserOrAPIKey_ValidKey(t *testing.T) {
	g := newAPIKeyTestEnv(t)
	u := newFixtureUser(t, g, "active")
	plain, _ := newFixtureKey(t, g, u.ID, nil)
	code, body, identity, _ := performProbe(t, []app.HandlerFunc{UserOrAPIKey()}, "/probe", hdr("X-API-Key", plain))
	assert.Equal(t, 200, code, body)
	assert.Equal(t, u.ID, identity["user_id"])
	assert.Equal(t, "api_key", identity["auth_type"])
}

func TestUserOrAPIKey_NoCredentials_401(t *testing.T) {
	newAPIKeyTestEnv(t)
	code, _, identity, _ := performProbe(t, []app.HandlerFunc{UserOrAPIKey()}, "/probe")
	assert.Equal(t, 401, code)
	assert.Empty(t, identity)
}

func TestUserOrAPIKey_InvalidJWT_401(t *testing.T) {
	newAPIKeyTestEnv(t)
	auth.SetJWTSecret("test-secret-for-apikey")
	t.Cleanup(func() { auth.SetJWTSecret("") })
	code, _, _, _ := performProbe(t, []app.HandlerFunc{UserOrAPIKey()}, "/probe",
		hdr("Authorization", "Bearer not.a.jwt"))
	assert.Equal(t, 401, code)
}

// --- OptionalIdentity（JWT 路为既有语义：无效 JWT 降级匿名） ---

func TestOptionalIdentity_MixedCredentials(t *testing.T) {
	g := newAPIKeyTestEnv(t)
	u := newFixtureUser(t, g, "active")
	plain, _ := newFixtureKey(t, g, u.ID, nil)

	// 有效 Key → 身份注入
	code, _, identity, _ := performProbe(t, OptionalIdentity(), "/probe", hdr("X-API-Key", plain))
	assert.Equal(t, 200, code)
	assert.Equal(t, u.ID, identity["user_id"])

	// 无效 JWT → 匿名放行（既有 OptionalAuth 语义，不 401）
	code, _, identity, _ = performProbe(t, OptionalIdentity(), "/probe", hdr("Authorization", "Bearer junk.token.here"))
	assert.Equal(t, 200, code)
	assert.Empty(t, identity)

	// 无凭证 → 匿名
	code, _, identity, _ = performProbe(t, OptionalIdentity(), "/probe")
	assert.Equal(t, 200, code)
	assert.Empty(t, identity)
}

// TestSanitizeHeaderValue 回显头消毒（2026-10-08 加固）：legacy 用户名未经
// 新白名单校验，出响应头前剥离控制字符（防 CR/LF 注入响应）。
func TestSanitizeHeaderValue(t *testing.T) {
	cases := map[string]string{
		"alice":       "alice",
		"a\r\nX-Evil": "aX-Evil",
		"a\x00b\x1fc": "abc",
		"空格 ok":       "空格 ok",
	}
	for in, want := range cases {
		if got := sanitizeHeaderValue(in); got != want {
			t.Errorf("sanitizeHeaderValue(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- AdminOrAdminKey（2026-10-14 P3：pb CLI 直传自动化通道）---

func TestAdminOrAdminKey_AdminKeyPass(t *testing.T) {
	g := newAPIKeyTestEnv(t)
	u := newFixtureUser(t, g, "active")
	require.NoError(t, g.Model(&model.User{}).Where("id = ?", u.ID).Update("role", "admin").Error)
	plain, _ := newFixtureKey(t, g, u.ID, nil)
	code, body, identity, _ := performProbe(t, []app.HandlerFunc{AdminOrAdminKey()}, "/probe", hdr("X-API-Key", plain))
	assert.Equal(t, 200, code, body)
	assert.Equal(t, u.ID, identity["user_id"])
	assert.Equal(t, "api_key", identity["auth_type"])
}

func TestAdminOrAdminKey_NonAdminKey403(t *testing.T) {
	g := newAPIKeyTestEnv(t)
	u := newFixtureUser(t, g, "active") // role=user——普通 Key 绝不进管理自动化面
	plain, _ := newFixtureKey(t, g, u.ID, nil)
	code, _, _, _ := performProbe(t, []app.HandlerFunc{AdminOrAdminKey()}, "/probe", hdr("X-API-Key", plain))
	assert.Equal(t, 403, code)
}

func TestAdminOrAdminKey_AdminJWTPass(t *testing.T) {
	g := newAPIKeyTestEnv(t)
	auth.SetJWTSecret("test-secret-for-apikey")
	t.Cleanup(func() { auth.SetJWTSecret("") })
	u := newFixtureUser(t, g, "active")
	require.NoError(t, g.Model(&model.User{}).Where("id = ?", u.ID).Update("role", "admin").Error)
	token, err := auth.GenerateToken(u.ID, u.Username, "admin")
	require.NoError(t, err)
	code, body, identity, _ := performProbe(t, []app.HandlerFunc{AdminOrAdminKey()}, "/probe",
		hdr("Authorization", "Bearer "+token))
	assert.Equal(t, 200, code, body)
	assert.Equal(t, u.ID, identity["user_id"])
}

func TestAdminOrAdminKey_NoCredentials401(t *testing.T) {
	newAPIKeyTestEnv(t)
	code, _, _, _ := performProbe(t, []app.HandlerFunc{AdminOrAdminKey()}, "/probe")
	assert.Equal(t, 401, code)
}
