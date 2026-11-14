package outbound

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/security"
)

// newTestClientWithDialGuard 走 buildClient 的 TargetOIDC 测试构造（dialGuard
// 策略生效后即带拨号复判）。全局配置由各用例自行钉死，本构造不碰配置。
func newTestClientWithDialGuard(t *testing.T) *http.Client {
	t.Helper()
	return newObservedClient(TargetOIDC, func(Target, Outcome, time.Duration) {})
}

// TestSSRFDial_BlocksPrivateAtDialTime（core#164 拨号时刻复判）：即便解析期
// 检查被 DNS rebinding 绕过，拨号到私网/环回仍拒绝；allow_private_networks=true
// 时放行（企业内网 IdP 场景）——与保存期校验同一开关，不引入第二策略。
//
// 判据为结构性（错误文案含"拒绝" + errors.Is(err, security.ErrEndpointURL)），
// 不依赖单条完整文案。
//
// 负对照（改错转红）：dialGuard 归零（回盲拨）⇒ 拒绝分支转红——错误退化成
// connection refused（不含"拒绝"、错误链断）；BlockedIP 恒放行同理。
func TestSSRFDial_BlocksPrivateAtDialTime(t *testing.T) {
	// 显式钉死严格口径（本用例判的就是默认拒绝；判据不随全局状态漂移）
	old := conf.GetGlobalConfig()
	conf.SetGlobalConfig(&conf.AppConfiguration{})
	defer conf.SetGlobalConfig(old)

	c := newTestClientWithDialGuard(t)
	_, err := c.Get("http://127.0.0.1:9/") // 环回：默认口径必须在拨号时刻被拒
	if err == nil || !strings.Contains(err.Error(), "拒绝") {
		t.Fatalf("loopback 拨号必须被拒, got %v", err)
	}
	if !errors.Is(err, security.ErrEndpointURL) {
		t.Fatalf("拨号拒绝错误链应可判 security.ErrEndpointURL, got %v", err)
	}

	// allow_private_networks=true 放行（内网 IdP）：同一开关流经 BlockedIP
	t.Run("allow_private_networks 放行", func(t *testing.T) {
		allowed := &conf.AppConfiguration{}
		allowed.Security.SSRF.AllowPrivateNetworks = true
		conf.SetGlobalConfig(allowed)
		defer conf.SetGlobalConfig(old)

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		resp, err := newTestClientWithDialGuard(t).Get(srv.URL)
		if err != nil {
			t.Fatalf("放行口径下环回应可拨通（假 IdP/内网端点形态）: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("放行口径下应拿到 200, got %d", resp.StatusCode)
		}
	})
}
