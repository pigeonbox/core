package handler_test

// SetOIDCService 热替换的持有器零化语义（core#81）：换密钥旧持有器清零、
// 同持有器重建不清现役密钥、nil 卸载清零。
import (
	"testing"

	"github.com/oekit/pkg/secretx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	oidcApp "github.com/pigeonbox/core/app/oidc"
	"github.com/pigeonbox/core/transport/http/handler"
)

func swapTestService(secret string) (*oidcApp.Service, *secretx.SecureString) {
	holder := secretx.NewSecureString(secret)
	return oidcApp.NewService(nil, oidcApp.Config{Enabled: true, ClientSecret: holder}), holder
}

func TestSetOIDCService_SwapClearsOldHolder(t *testing.T) {
	t.Cleanup(func() { handler.SetOIDCService(nil) })

	svcA, holderA := swapTestService("swap-old-88fa")
	handler.SetOIDCService(svcA)

	// 换新持有器（换密钥语义）⇒ 旧持有器零化。
	svcB, holderB := swapTestService("swap-new-11cc")
	handler.SetOIDCService(svcB)
	require.Equal(t, "swap-new-11cc", holderB.Get(), "新持有器照常服役")
	assert.Empty(t, holderA.Get(), "旧持有器必须被零化")
}

func TestSetOIDCService_SameHolderRebuildKeepsLive(t *testing.T) {
	t.Cleanup(func() { handler.SetOIDCService(nil) })

	// 未改 secret 的重建（conf/mirror/服务三方同指针）⇒ 共享持有器绝不能清。
	svcA, shared := swapTestService("live-secret-3b7d")
	handler.SetOIDCService(svcA)
	svcB := oidcApp.NewService(nil, oidcApp.Config{Enabled: true, ClientSecret: shared})
	handler.SetOIDCService(svcB)
	assert.Equal(t, "live-secret-3b7d", shared.Get(), "同持有器重建不得清现役密钥")
}

func TestSetOIDCService_UnloadClears(t *testing.T) {
	svc, holder := swapTestService("unload-9e01")
	handler.SetOIDCService(svc)
	handler.SetOIDCService(nil)
	assert.Empty(t, holder.Get(), "卸载路径旧持有器也要零化")
}
