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
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/pigeonbox/core/pkg/auth"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/oekit/pkg/httpjson"
	"github.com/oekit/concurrency/singleflight"
)

// ErrAccountDisabled 匹配命中的本地账号已被禁用（回调不发 JWT，2026-10-08 加固）
var ErrAccountDisabled = errors.New("OIDC 登录被拒绝：账号已被禁用")

// Config OIDC 配置（conf.SecurityConfig.OIDC）
type Config struct {
	Enabled          bool   `mapstructure:"enabled"`
	Issuer           string `mapstructure:"issuer"` // 如 https://idp.example.com/realms/pb
	ClientID         string `mapstructure:"client_id"`
	ClientSecret     string `mapstructure:"client_secret"`
	Scopes           string `mapstructure:"scopes"`            // 默认 "openid profile email"
	FrontendCallback string `mapstructure:"frontend_callback"` // 默认 /#/oidc/callback
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

	mu     sync.RWMutex
	disc   *discovery
	discAt time.Time
	discSF *singleflight.Flight[*discovery] // 缓存过期时的并发拉取合并（防登录风暴打 IdP）

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
	return &Service{
		cfg:      cfg,
		client:   &http.Client{Timeout: 15 * time.Second},
		discSF:   singleflight.New[*discovery](),
		userRepo: dao.NewUserRepository(gormDB),
	}
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

// discover 发现端点（缓存 1 小时；过期后并发回调经 singleflight 合并为一次拉取）
func (s *Service) discover(ctx context.Context) (*discovery, error) {
	if d := s.cachedDiscovery(); d != nil {
		return d, nil
	}
	d, err := s.discSF.Do(ctx, "discovery", func(ctx context.Context) (*discovery, error) {
		// 双检:排队等待期间可能已被同批调用刷新
		if d := s.cachedDiscovery(); d != nil {
			return d, nil
		}
		issuer := strings.TrimSuffix(s.cfg.Issuer, "/")
		wellKnown := issuer + "/.well-known/openid-configuration"
		var d discovery
		if err := httpjson.DoJSON(ctx, s.client, httpjson.Request{URL: wellKnown}, &d); err != nil {
			return nil, fmt.Errorf("OIDC discovery 失败: %w", err)
		}
		if d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" {
			return nil, errors.New("OIDC discovery 缺少必要端点")
		}
		s.mu.Lock()
		s.disc = &d
		s.discAt = time.Now()
		s.mu.Unlock()
		return &d, nil
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}

// cachedDiscovery 返回未过期的缓存发现结果（nil = 未命中）。
func (s *Service) cachedDiscovery() *discovery {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.disc != nil && time.Since(s.discAt) < time.Hour {
		return s.disc
	}
	return nil
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

// GenerateNonce 生成一次性的浏览器绑定 nonce（login CSRF 防线，2026-10-08 加固）：
// 登录起点下发 HttpOnly Cookie，state 的 HMAC 绑定该 nonce——回调时 Cookie 与
// state 必须同源同流，第三方拿"自己的 code+state"诱导受害者浏览器回调时，
// 受害者没有匹配的 nonce Cookie，登录 CSRF 不成立。
func GenerateNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// signState HMAC 签名（时间戳+nonce，1 小时有效；无需服务端会话）
func (s *Service) signState(ts int64, nonce string) string {
	mac := hmac.New(sha256.New, []byte(s.cfg.ClientSecret))
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
		return "", errors.New("state 校验失败（过期/伪造/缺少浏览器绑定），请重试登录")
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
	form.Set("client_secret", s.cfg.ClientSecret)
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
		username = "oidc-" + claims.Sub[:8]
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
	// 随机密码（OIDC 用户不可走密码登录）
	pw := make([]byte, 24)
	_, _ = rand.Read(pw)
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
		user.Email = fmt.Sprintf("oidc-%s@oidc.local", claims.Sub[:8])
	}
	if err := s.userRepo.Create(ctx, user); err != nil {
		return nil, fmt.Errorf("OIDC 用户创建失败: %w", err)
	}
	return user, nil
}
