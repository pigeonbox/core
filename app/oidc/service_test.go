package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/oekit/pkg/secretx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/pkg/auth"
	"github.com/pigeonbox/core/repo/db/model"
)

// fakeIdP 假 OIDC Provider：discovery + token + userinfo 三端点
func fakeIdP(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	tokHits := 0
	mux := http.NewServeMux()
	// baseURL 在 NewServer 后回填（discovery 端点动态引用真实监听地址）
	baseURL := ""
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 baseURL,
			"authorization_endpoint": baseURL + "/auth",
			"token_endpoint":         baseURL + "/token",
			"userinfo_endpoint":      baseURL + "/userinfo",
		})
	})
	// authorization_endpoint 不经服务端（浏览器跳转），此处不模拟
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		tokHits++
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(400)
			return
		}
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "the-code" {
			w.WriteHeader(400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "at-123"})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at-123" {
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sub":                "user-sub-42",
			"email":              "alice@example.com",
			"email_verified":     true,
			"preferred_username": "alice",
			"name":               "Alice",
		})
	})
	srv := httptest.NewServer(mux)
	baseURL = srv.URL
	t.Cleanup(srv.Close)
	return srv, &tokHits
}

func newOIDCDB(t *testing.T) *gorm.DB {
	t.Helper()
	gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, gormDB.AutoMigrate(&model.User{}))
	sqlDB, err := gormDB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return gormDB
}

func TestOIDC_FullFlow(t *testing.T) {
	oidcDB := newOIDCDB(t)
	auth.SetJWTSecret("oidc-test-secret-0123456789abcdef")
	srv, tokHits := fakeIdP(t)

	// Issuer 用真实 http 地址（discovery 走网络），其余端点为假 IdP 固定值
	svc := NewService(oidcDB, Config{
		Enabled:      true,
		Issuer:       srv.URL,
		ClientID:     "client-1",
		ClientSecret: secretx.NewSecureString("secret-1"),
	})

	// 1. 登录 URL：含授权端点 + HMAC state（绑定浏览器 nonce）
	nonce, err := GenerateNonce()
	require.NoError(t, err)
	loginURL, err := svc.LoginURL(context.Background(), "http://pb.local", nonce)
	require.NoError(t, err)
	assert.Contains(t, loginURL, srv.URL+"/auth?")
	assert.Contains(t, loginURL, "client_id=client-1")
	state := loginURL[strings.Index(loginURL, "state=")+len("state="):]

	// 2. 伪造 state / nonce 不匹配（login CSRF 防线）拒绝
	_, err = svc.ExchangeCallback(context.Background(), "http://pb.local", "the-code", "1.deadbeef", nonce)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "state")
	_, err = svc.ExchangeCallback(context.Background(), "http://pb.local", "the-code", state, "attacker-nonce")
	require.Error(t, err, "state 绑定 nonce，错配必须拒绝")
	_, err = svc.ExchangeCallback(context.Background(), "http://pb.local", "the-code", state, "")
	require.Error(t, err, "缺失 nonce Cookie 必须拒绝")

	// 3. 正常回调：code 换 token → userinfo → 建号 → 本站 JWT
	token, err := svc.ExchangeCallback(context.Background(), "http://pb.local", "the-code", state, nonce)
	require.NoError(t, err)
	assert.NotEmpty(t, token)
	assert.Equal(t, 1, *tokHits)

	// 4. 建出的用户：sub 精确匹配 + 字段正确
	claims, err := auth.ParseToken(token)
	require.NoError(t, err)
	var u model.User
	require.NoError(t, oidcDB.Where("oidc_sub = ?", "user-sub-42").First(&u).Error)
	assert.Equal(t, claims.UserID, u.ID)
	assert.Equal(t, "alice", u.Username)
	assert.Equal(t, "alice@example.com", u.Email)

	// 5. 二次登录：按 sub 命中既有用户（不重复建号）
	token2, err := svc.ExchangeCallback(context.Background(), "http://pb.local", "the-code", svc.signState(time.Now().Unix(), "n2"), "n2")
	require.NoError(t, err)
	claims2, err := auth.ParseToken(token2)
	require.NoError(t, err)
	assert.Equal(t, claims.UserID, claims2.UserID)
	var cnt int64
	oidcDB.Model(&model.User{}).Count(&cnt)
	assert.Equal(t, int64(1), cnt)
}

func TestOIDC_EmailMatchBindsSub(t *testing.T) {
	oidcDB := newOIDCDB(t)
	auth.SetJWTSecret("oidc-test-secret-0123456789abcdef")
	srv, _ := fakeIdP(t)
	svc := NewService(oidcDB, Config{Enabled: true, Issuer: srv.URL, ClientID: "c", ClientSecret: secretx.NewSecureString("s")})

	// 先以密码注册同邮箱用户
	existing := &model.User{
		Username: "alice-local", Email: "alice@example.com",
		PasswordHash: "x", Role: "user", Status: "active",
	}
	require.NoError(t, oidcDB.Create(existing).Error)

	token, err := svc.ExchangeCallback(context.Background(), "http://pb.local", "the-code", svc.signState(time.Now().Unix(), "n3"), "n3")
	require.NoError(t, err)
	claims, err := auth.ParseToken(token)
	require.NoError(t, err)
	assert.Equal(t, existing.ID, claims.UserID, "同邮箱应绑定既有账号而非建新号")

	var u model.User
	require.NoError(t, oidcDB.First(&u, existing.ID).Error)
	assert.Equal(t, "user-sub-42", u.OidcSub, "登录时应回写 sub 绑定")
}

// TestOIDC_BannedUserRejected 封禁用户回调拒发 JWT（2026-10-08 加固）：
// 密码登录在同位检查拒绝非 active 用户，OIDC 匹配命中不得绕过。
func TestOIDC_BannedUserRejected(t *testing.T) {
	oidcDB := newOIDCDB(t)
	auth.SetJWTSecret("oidc-test-secret-0123456789abcdef")
	srv, _ := fakeIdP(t)
	svc := NewService(oidcDB, Config{Enabled: true, Issuer: srv.URL, ClientID: "c", ClientSecret: secretx.NewSecureString("s")})

	// 已有 sub 绑定但被封禁的账号
	require.NoError(t, oidcDB.Create(&model.User{
		Username: "banned", OidcSub: "user-sub-42",
		PasswordHash: "x", Role: "user", Status: "disabled",
	}).Error)

	_, err := svc.ExchangeCallback(context.Background(), "http://pb.local", "the-code", svc.signState(time.Now().Unix(), "n"), "n")
	require.ErrorIs(t, err, ErrAccountDisabled)
}

// ---- core#86：随机源失败的错误上抛（原实现吞错 ⇒ 静默发弱 nonce） ----

// boomReader 必然失败的随机源注入桩（core#86 用例专用）。
type boomReader struct{}

func (boomReader) Read(p []byte) (int, error) { return 0, errors.New("boom: 熵源故障") }

// TestGenerateNonce_RandFailurePropagates（core#86）：随机源失败必须显式上抛，
// 绝不静默发弱 nonce——原实现 `_, _ = rand.Read(b)` 吞错 ⇒ 熵源故障时发出全 0
// nonce，state 绑定的 login CSRF 防线失效。
//
// 负对照：若把 GenerateNonce 改回吞错形态（忽略 err 用半截产物顶包），下面的
// require.Error 会转红（err=nil）；若把半截产物连同错误一起返回，assert.Empty
// 会转红——两条判据各自锁死一种复活形态。
func TestGenerateNonce_RandFailurePropagates(t *testing.T) {
	// 基线：默认随机源可用 ⇒ 正常出 32 位 hex（证明判据走的是注入路径，非恒红）
	nonce, err := GenerateNonce()
	require.NoError(t, err)
	assert.Len(t, nonce, 32)

	// 注入必然失败的 reader ⇒ (空串, error)，弱 nonce 不出闸
	orig := nonceRandReader
	nonceRandReader = boomReader{}
	defer func() { nonceRandReader = orig }()

	got, err := GenerateNonce()
	require.Error(t, err, "随机源失败必须上抛（吞错形态会在这里假绿）")
	assert.Contains(t, err.Error(), "boom", "错误链必须携带注入的失败原因")
	assert.Empty(t, got, "错误路径不得返回半截产物（弱 nonce 出闸即防线失效）")
}
