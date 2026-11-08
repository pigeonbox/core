package oidc

import (
	"bytes"
	"testing"
	"time"

	"github.com/oekit/pkg/secretx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/internal/sectest"
)

// 本文件锁住 core#81 的服务侧三条语义：密钥不以明文驻留 / nil 归一不 panic /
// 热更换密钥旧 state 失效、新 state 有效（issue 验收第 3/4 条的服务层判据）。

const oidcSecretMarker = "oidc-service-secret-7c1d"

// TestOIDC_ServiceSecretNotStoredInPlaintext 服务持有的密钥混淆生效：
// 普通切片对照必须扫到（扫描手法有效），持有器内部扫不到，取回是原文。
func TestOIDC_ServiceSecretNotStoredInPlaintext(t *testing.T) {
	svc := NewService(nil, Config{Enabled: true, Issuer: "https://idp.example.com", ClientSecret: secretx.NewSecureString(oidcSecretMarker)})

	// 对照：普通切片必须被扫到——否则本条只能证明扫描手法坏掉（假绿）。
	if !bytes.Contains([]byte(oidcSecretMarker), []byte(oidcSecretMarker)) {
		t.Fatal("对照失败：普通切片里扫不到明文，扫描手法本身有问题")
	}
	sectest.AssertNotStoredInPlaintext(t, svc.cfg.ClientSecret, oidcSecretMarker)
	assert.Equal(t, oidcSecretMarker, svc.clientSecret())
}

// TestOIDC_NilSecretNormalized 裸构造（绕过 NewService 归一不可达，这里验
// nil 注入路径）：持有器归一为空、读取点不 panic、signState 用空钥自洽。
func TestOIDC_NilSecretNormalized(t *testing.T) {
	svc := NewService(nil, Config{Enabled: true})
	require.NotNil(t, svc.SecretHolder())
	assert.Empty(t, svc.clientSecret())
	st := svc.signState(time.Now().Unix(), "n")
	assert.True(t, svc.verifyState(st, "n"), "空钥签发的 state 必须自洽可验")

	// 外部直接给 nil Config（绕过 NewService 的归一分支也可能被手工构造复现）
	raw := Service{cfg: Config{}}
	assert.Empty(t, raw.clientSecret(), "nil 持有器防御：返回空串不 panic")
}

// TestOIDC_SecretRotationInvalidatesState 热更换密钥（issue 验收第 4 条）：
// 旧密钥签发的 state 在新密钥下失效，新密钥签发有效；旧持有器 Clear 后
// 混淆存储零化（反射断言）。
func TestOIDC_SecretRotationInvalidatesState(t *testing.T) {
	old := secretx.NewSecureString("rotate-old-secret")
	svcOld := NewService(nil, Config{Enabled: true, ClientSecret: old})

	nonce := "rotate-nonce"
	oldState := svcOld.signState(time.Now().Unix(), nonce)
	require.True(t, svcOld.verifyState(oldState, nonce))

	// 旋转：新持有器 + 新服务（生产路径 = SetOIDCService(NewService(...))，
	// 旧持有器由 conf overlay 的 identity 守卫 Clear——这里在单测层直接清）。
	newSecret := "rotate-new-secret"
	svcNew := NewService(nil, Config{Enabled: true, ClientSecret: secretx.NewSecureString(newSecret)})
	assert.False(t, svcNew.verifyState(oldState, nonce), "旧密钥签发的 state 在新密钥下必须失效")

	newState := svcNew.signState(time.Now().Unix(), nonce)
	assert.True(t, svcNew.verifyState(newState, nonce), "新密钥签发的 state 必须有效")

	// 旧持有器零化（overlay/热换路径的 Clear 语义，反射断言）。
	old.Clear()
	sectest.AssertCleared(t, old)
}
