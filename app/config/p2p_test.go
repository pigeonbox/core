package config

import (
	"testing"

	"github.com/pigeonbox/core/conf"
)

// TestValidateSystemConfigP2PFederation p2p.federation 段校验:URL 仅 http/https、
// registry 支持逗号多主备、熵门槛范围。写库前最后一道防线(同 notify/oidc 先例)。
func TestValidateSystemConfigP2PFederation(t *testing.T) {
	ok := func() *SystemConfig {
		return &SystemConfig{P2P: &P2PSection{Federation: &conf.FederationConfig{
			Enabled:     true,
			RegistryURL: "http://p2p1:12346,https://p2p2:56788",
			PublicURL:   "https://share.example.com",
		}}}
	}
	if err := validateSystemConfig(ok()); err != nil {
		t.Fatalf("合法配置被拒: %v", err)
	}
	badScheme := ok()
	badScheme.P2P.Federation.RegistryURL = "ftp://p2p:12346"
	if err := validateSystemConfig(badScheme); err == nil {
		t.Fatal("ftp scheme 应被拒")
	}
	badPub := ok()
	badPub.P2P.Federation.PublicURL = "javascript:alert(1)"
	if err := validateSystemConfig(badPub); err == nil {
		t.Fatal("非法 public_url 应被拒")
	}
	badEntropy := ok()
	badEntropy.P2P.Federation.AnnounceMinEntropyBits = -1
	if err := validateSystemConfig(badEntropy); err == nil {
		t.Fatal("负熵门槛应被拒")
	}
	if err := validateSystemConfig(&SystemConfig{P2P: &P2PSection{}}); err != nil {
		t.Fatalf("P2P 段无 federation(nil)应合法: %v", err)
	}
}
