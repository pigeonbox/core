// Package oidc 实现 OIDC 单点登录（P2，stdlib-only，无第三方依赖）。
//
// 流程（Authorization Code）：
//  1. GET /api/v1/user/oidc/login → 302 到 IdP 授权页（state 为 HMAC 签名时间戳，防 CSRF）
//  2. IdP 回调 GET /api/v1/user/oidc/callback?code&state → 验 state → 换 token → 拉 userinfo
//  3. 按 (issuer, sub) 匹配本地用户，其次按 email，未命中且允许注册则建号
//  4. 签发本站 JWT，302 回前端 /#/oidc/callback?token=xxx
package oidc

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/oekit/kitx/uidgen"
	"github.com/oekit/pkg/cache"
	"github.com/oekit/pkg/httpjson"
	"github.com/oekit/pkg/secretx"
	"github.com/pigeonbox/core/pkg/auth"
	"github.com/pigeonbox/core/pkg/outbound"
	"github.com/pigeonbox/core/pkg/utils"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
)

// discoveryTTL 发现文档缓存窗。发现文档的端点集几乎不变（IdP 换端点是运维事件），
// stale-if-error 档兜 IdP 抖动：回源失败时回退已过期历史真值，登录面不因 IdP
// 的 discovery 抖动整面不可用。
const discoveryTTL = time.Hour

// ErrAccountDisabled 匹配命中的本地账号已被禁用（回调不发 JWT，2026-10-08 加固）
var ErrAccountDisabled = errors.New("OIDC 登录被拒绝：账号已被禁用")

// ErrEmailInvalid IdP 返回的 email claim 格式非法（core#119 同族收口）：
// JIT 建号会把该值落库并流入 SMTP To 头 ⇒ 校验失败**显式拒绝登录**，
// 绝不把注入类邮箱写进库/参与匹配（不静默降级占位邮箱——那又是静默改义）。
var ErrEmailInvalid = errors.New("OIDC 登录被拒绝：身份提供方返回的邮箱格式无效")

// ErrStateInvalid state 校验失败（过期/伪造/缺少浏览器绑定）。
//
// 独立哨兵（2026-10-26 审计 P2）：transport 层要把 OIDC 失败映射成**可公开**
// 的短文案 + 稳定短码 ⇒ 需要能机器判定「是 state 问题」而不是靠字符串匹配
// （字符串匹配会被上游改文案悄悄打断）。
var ErrStateInvalid = errors.New("state 校验失败（过期/伪造/缺少浏览器绑定），请重试登录")

// Config OIDC 配置（conf.SecurityConfig.OIDC）
//
// ClientSecret 持 conf 解码产出的持有器（core#81：conf.SecretStringDecodeHook
// 直接填 *SecureString）——装配层传递指针（bootstrap 三处 NewService 均从
// conf/mirror 取持有器），服务内部只经 Get() 取值，明文不在本包驻留。
type Config struct {
	Enabled          bool                  `mapstructure:"enabled"`
	Issuer           string                `mapstructure:"issuer"` // 如 https://idp.example.com/realms/pb
	ClientID         string                `mapstructure:"client_id"`
	ClientSecret     *secretx.SecureString `mapstructure:"client_secret"`
	Scopes           string                `mapstructure:"scopes"`            // 默认 "openid profile email"
	FrontendCallback string                `mapstructure:"frontend_callback"` // 默认 /#/oidc/callback
}

// discovery 端点发现缓存
type discovery struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

// Service OIDC 登录服务
type Service struct {
	cfg    Config
	client *http.Client

	// disc 发现文档单值缓存（oekit/pkg v0.37.0 SingleValue 红利收编）：TTL/
	// 并发单飞合并/stale-if-error 全由库钉死，替代手搓 mu+disc+discAt+
	// singleflight 双检三件套。
	disc *cache.SingleValue[discovery]

	userRepo *dao.UserRepository
}

// NewService 构建服务
func NewService(gormDB *gorm.DB, cfg Config) *Service {
	if cfg.Scopes == "" {
		cfg.Scopes = "openid profile email"
	}
	if cfg.FrontendCallback == "" {
		cfg.FrontendCallback = "/#/oidc/callback"
	}
	if cfg.ClientSecret == nil {
		// 未注入（装配层持有器为 nil，如裸 conf 零值）⇒ 归一为空持有器：
		// 与旧 string 零值同语义（signState/换 token 用空钥，登录必然失败但
		// 不 panic），读取点不再做 nil 分叉。
		cfg.ClientSecret = secretx.NewSecureString("")
	}
	s := &Service{
		cfg: cfg,
		// 15s＝注册表统一超时基准（core#87 步骤 1）；discovery 的 SingleValue
		// 缓存（TTL/单飞/stale-if-error）不受影响。
		client:   outbound.New(outbound.TargetOIDC),
		userRepo: dao.NewUserRepository(gormDB),
	}
	// Source 返回 discovery 值类型；本形态恒有真值（端点缺失在 fetch 内判错
	// 返回 err，不落 null marker）。
	s.disc = cache.NewSingleValue(cache.SingleValueConfig[discovery]{
		TTL: discoveryTTL,
		Source: func(ctx context.Context) (*discovery, error) {
			d, err := s.fetchDiscovery(ctx)
			if err != nil {
				return nil, err
			}
			return &d, nil
		},
		// 单值条目不值得常驻清扫协程；stale-if-error 兜 IdP 抖动——
		// 回源失败时回退已过期历史真值，登录面不因 discovery 瞬断整面不可用。
		NoBackgroundSweep: true,
		StaleIfError:      true,
	})
	return s
}

// Enabled 是否启用
func (s *Service) Enabled() bool { return s.cfg.Enabled && s.cfg.Issuer != "" && s.cfg.ClientID != "" }

// RedirectURI 回调地址（本站）
func (s *Service) RedirectURI(baseURL string) string {
	return strings.TrimSuffix(baseURL, "/") + "/api/v1/user/oidc/callback"
}

// FrontendCallback 前端回调地址（带 token= 前缀；hash 路由 query）
func (s *Service) FrontendCallback() string {
	sep := "?"
	if strings.Contains(s.cfg.FrontendCallback, "?") {
		sep = "&"
	}
	return s.cfg.FrontendCallback + sep + "token="
}

// discover 取发现文档（SingleValue 全托管：命中直接返回；过期后并发经内置
// 单飞合并为一次回源；回源失败 stale-if-error 回退历史真值——IdP 抖动不再
// 让登录面整面不可用，pkg v0.37.0 红利收编）。
func (s *Service) discover(ctx context.Context) (*discovery, error) {
	return s.disc.Get(ctx)
}

// fetchDiscovery 实际回源：拉 discovery 文档并校验必要端点（错误不缓存，
// 端点缺失不落 null marker——本形态恒有真值，取不到即失败）。
func (s *Service) fetchDiscovery(ctx context.Context) (discovery, error) {
	issuer := strings.TrimSuffix(s.cfg.Issuer, "/")
	wellKnown := issuer + "/.well-known/openid-configuration"
	var d discovery
	if err := httpjson.DoJSON(ctx, s.client, httpjson.Request{URL: wellKnown}, &d); err != nil {
		return d, fmt.Errorf("OIDC discovery 回源失败: %w", err)
	}
	if d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" {
		return d, errors.New("OIDC discovery 缺少必要端点")
	}
	return d, nil
}

// TestDiscovery 管理端「测试连接」：验证 issuer 的 discovery 端点可达且合法。
func (s *Service) TestDiscovery(ctx context.Context) error {
	if s.cfg.Issuer == "" {
		return errors.New("issuer 未配置")
	}
	wellKnown := strings.TrimSuffix(s.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	var d struct {
		AuthorizationEndpoint string `json:"authorization_endpoint"`
		TokenEndpoint         string `json:"token_endpoint"`
	}
	if err := httpjson.DoJSON(ctx, s.client, httpjson.Request{URL: wellKnown}, &d); err != nil {
		return fmt.Errorf("discovery 不可用: %w", err)
	}
	if d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" {
		return errors.New("discovery 缺少 authorization/token 端点")
	}
	return nil
}

// oidcNonceEntropyBytes 登录 CSRF nonce 的熵预算：16 字节 = 128 bit 熵 ⇒ 2^128
// 防碰撞/防猜测预算（core#86 收编 kitx/uidgen 时定长于此；一次性 Cookie，回调
// 即作废，无需跨进程唯一）。
const oidcNonceEntropyBytes = 16

// nonceRandReader 随机源 seam（默认 crypto/rand.Reader；测试注入必然失败的
// reader 验证错误上抛——core#86：随机源失败绝不能静默发弱 nonce）。
var nonceRandReader io.Reader = rand.Reader

// GenerateNonce 生成一次性的浏览器绑定 nonce（login CSRF 防线，2026-10-08 加固）：
// 登录起点下发 HttpOnly Cookie，state 的 HMAC 绑定该 nonce——回调时 Cookie 与
// state 必须同源同流，第三方拿"自己的 code+state"诱导受害者浏览器回调时，
// 受害者没有匹配的 nonce Cookie，登录 CSRF 不成立。
//
// 随机源失败**显式上抛**（core#86：原实现吞掉 crypto/rand 的读取错误 ⇒ 熵源
// 故障时静默发出全 0 弱 nonce，state 绑定形同虚设）——调用方（OIDC 登录起点
// handler）收到错误即拒绝登录，不发弱 nonce。随机源经 nonceRandReader seam
// 注入（From 形态=测试注入口）。
func GenerateNonce() (string, error) {
	return uidgen.RandomHexFrom(nonceRandReader, oidcNonceEntropyBytes)
}

// clientSecret 取当前密钥明文（每次在持有器独立副本上解出；nil 防御——
// 外部手工构造 Config 可绕过 NewService 的归一）。
func (s *Service) clientSecret() string {
	if s.cfg.ClientSecret == nil {
		return ""
	}
	return s.cfg.ClientSecret.Get()
}

// SecretHolder 内部密钥持有器（热替换路径判定新旧服务是否共享同一持有器用；
// 勿用于取值——取值走 clientSecret()/Get()）。
func (s *Service) SecretHolder() *secretx.SecureString { return s.cfg.ClientSecret }

// ClearSecret 零化本服务持有的密钥（热替换路径用）：旧服务下线前把混淆存储
// 清零，不留 freed-heap 明文。与 pkg/auth.SetJWTSecret 的热更语义同款
// （core#73 模板：Set 内部先零化旧值）。
func (s *Service) ClearSecret() {
	if s.cfg.ClientSecret != nil {
		s.cfg.ClientSecret.Clear()
	}
}

// signState HMAC 签名（时间戳+nonce，1 小时有效；无需服务端会话）
func (s *Service) signState(ts int64, nonce string) string {
	mac := hmac.New(sha256.New, []byte(s.clientSecret()))
	_, _ = fmt.Fprintf(mac, "oidc-state:%d:%s", ts, nonce) // hash.Write 恒返回 nil
	return fmt.Sprintf("%d.%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

func (s *Service) verifyState(state, nonce string) bool {
	var ts int64
	var sig string
	if _, err := fmt.Sscanf(state, "%d.%s", &ts, &sig); err != nil {
		return false
	}
	if time.Since(time.Unix(ts, 0)) > time.Hour {
		return false
	}
	return hmac.Equal([]byte(s.signState(ts, nonce)), []byte(state))
}

// LoginURL 构建授权跳转地址（nonce 由调用方生成并下发 Cookie，见 GenerateNonce）
func (s *Service) LoginURL(ctx context.Context, baseURL, nonce string) (string, error) {
	d, err := s.discover(ctx)
	if err != nil {
		return "", err
	}
	ts := time.Now().Unix()
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", s.cfg.ClientID)
	q.Set("redirect_uri", s.RedirectURI(baseURL))
	q.Set("scope", s.cfg.Scopes)
	q.Set("state", s.signState(ts, nonce))
	// PKCE 可选：部分 IdP 对公共客户端要求；机密客户端可省略（v1 不启用）
	return d.AuthorizationEndpoint + "?" + q.Encode(), nil
}

// idClaims userinfo 载荷
type idClaims struct {
	Sub               string `json:"sub"`
	Email             string `json:"email"`
	EmailVerified     bool   `json:"email_verified"`
	PreferredUsername string `json:"preferred_username"`
	Name              string `json:"name"`
}

// ExchangeCallback 处理回调：code → token → userinfo → 本地用户 → JWT。
// nonce 为登录起点下发的浏览器绑定值（Cookie 回读），state 校验必需。
func (s *Service) ExchangeCallback(ctx context.Context, baseURL, code, state, nonce string) (string, error) {
	if nonce == "" || !s.verifyState(state, nonce) {
		return "", ErrStateInvalid
	}
	d, err := s.discover(ctx)
	if err != nil {
		return "", err
	}

	// 1. 换 token
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", s.RedirectURI(baseURL))
	form.Set("client_id", s.cfg.ClientID)
	form.Set("client_secret", s.clientSecret())
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := httpjson.DoJSON(ctx, s.client, httpjson.Request{
		Method: http.MethodPost,
		URL:    d.TokenEndpoint,
		Body:   []byte(form.Encode()),
		Header: func(h http.Header) { h.Set("Content-Type", "application/x-www-form-urlencoded") },
	}, &tok); err != nil {
		return "", fmt.Errorf("token 交换失败: %w", err)
	}
	if tok.AccessToken == "" {
		return "", errors.New("token 响应缺少 access_token")
	}

	// 2. 拉 userinfo
	var claims idClaims
	if err := httpjson.DoJSON(ctx, s.client, httpjson.Request{
		URL: d.UserinfoEndpoint,
		Header: func(h http.Header) {
			h.Set("Authorization", "Bearer "+tok.AccessToken)
		},
	}, &claims); err != nil {
		return "", fmt.Errorf("userinfo 拉取失败: %w", err)
	}
	if claims.Sub == "" {
		return "", errors.New("userinfo 缺少 sub")
	}

	// 3. 匹配/创建本地用户
	user, err := s.matchOrCreate(ctx, claims)
	if err != nil {
		return "", err
	}

	// 4. 签发本站 JWT
	return auth.GenerateToken(user.ID, user.Username, user.Role)
}

// matchOrCreate (issuer,sub) → email → 建号
//
// 邮箱匹配与自动建号均要求 IdP 侧 email_verified=true（2026-10-05 审计 P1 修复）：
// 此前只要 userinfo 带 email 就按邮箱绑定本地账号——自建 IdP（Keycloak 等）
// 若允许未验证邮箱注册，攻击者用受害者邮箱在 IdP 注册即可接管本站账号
// （pre-hijack）。email_verified 缺失/false 时仅 (issuer,sub) 精确匹配可用。
// 匹配命中的既有账号必须处于 active（2026-10-08 加固）：此前封禁用户经 OIDC
// 回调仍可换得新 JWT（密码登录在同位检查拒绝）。
func (s *Service) matchOrCreate(ctx context.Context, claims idClaims) (*model.User, error) {
	// 按 sub 精确匹配（身份链由 IdP 保证，无条件信任）
	if u, err := s.userRepo.GetByOIDCSub(ctx, claims.Sub); err == nil && u != nil {
		if u.Status != "active" {
			return nil, ErrAccountDisabled
		}
		return u, nil
	}
	// 按邮箱匹配（用户先以密码注册、后用 OIDC 登录的场景）——仅限已验证邮箱
	if claims.Email != "" && claims.EmailVerified {
		// core#119 同族收口：email claim 会按此值 JIT 建号落库（下方）并流入
		// SMTP To 头 ⇒ 与注册面同款 utils.ValidateEmail 严格校验，非法即显式
		// 失败（ErrEmailInvalid）。未验证邮箱不受此闸（本就不落库，走占位邮箱）。
		if err := utils.ValidateEmail(claims.Email); err != nil {
			return nil, ErrEmailInvalid
		}
		if u, err := s.userRepo.GetByEmail(ctx, claims.Email); err == nil && u != nil {
			if u.Status != "active" {
				return nil, ErrAccountDisabled
			}
			// 绑定 sub，后续走精确匹配
			_ = s.userRepo.UpdateColumns(ctx, u.ID, map[string]interface{}{"oidc_sub": claims.Sub})
			return u, nil
		}
	}
	// 建号（用户名去重）
	username := claims.PreferredUsername
	if username == "" {
		username = claims.Name
	}
	if username == "" && claims.Email != "" {
		username = strings.SplitN(claims.Email, "@", 2)[0]
	}
	if username == "" {
		username = "oidc-" + oidcSubFingerprint(claims.Sub)
	}
	base := username
	for i := 1; ; i++ {
		if _, err := s.userRepo.GetByUsername(ctx, username); err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				break
			}
			return nil, err
		}
		username = fmt.Sprintf("%s-%d", base, i)
	}
	// （core#86）原此处有一段「随机密码」死代码：手搓生成 24 字节随机数填充
	// pw 后从未消费（user 不落 PasswordHash，密码登录由同位检查拒绝 OIDC
	// 用户），且吞错；整体删除，行为零变化。
	user := &model.User{
		Username: username,
		Email:    claims.Email,
		Nickname: claims.Name,
		Role:     "user",
		Status:   "active",
		OidcSub:  claims.Sub,
	}
	if claims.Email == "" || !claims.EmailVerified {
		// 未验证邮箱不落库（与邮箱匹配同门槛，防占位邮箱后续被用于接管）：
		// email 唯一索引不接受空串重复，以 sub 生成占位
		user.Email = fmt.Sprintf("oidc-%s@oidc.local", oidcSubFingerprint(claims.Sub))
	}
	if err := s.userRepo.Create(ctx, user); err != nil {
		return nil, fmt.Errorf("OIDC 用户创建失败: %w", err)
	}
	return user, nil
}

// oidcSubFingerprint 取 sub 的稳定短指纹（16 个 hex 字符），用作自动建号时的
// 用户名后缀与邮箱占位。
//
// ⚠️ 越界防护（2026-10-25 审计 A8）：此前直接 `claims.Sub[:8]` ⇒ IdP 返回
// **长度 < 8** 的 sub（如 "a1"）⇒ **切片越界 panic** ⇒ 该 IdP 的所有 OIDC
// 登录 500（hertz Recovery 只把它转成 500，不救功能）。sub 的唯一约束是
// **非空**（见上），长度不保证 ⇒ 不能按固定长度切片。
//
// 顺带修掉**碰撞**：取前 8 位时两个 sub 只要前 8 位相同（如
// "user-0001-xxx" 与 "user-0001-yyy"）就会撞用户名/占位邮箱 ⇒ 后者建号失败
// （唯一索引冲突）。改用 sha256 指纹 ⇒ 与长度无关、且不同 sub 不碰撞。
func oidcSubFingerprint(sub string) string {
	sum := sha256.Sum256([]byte(sub))
	return hex.EncodeToString(sum[:8])
}
