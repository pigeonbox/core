package bootstrap

import (
	"errors"
	"path/filepath"
	"testing"

	federationApp "github.com/pigeonbox/core/app/federation"
	"github.com/pigeonbox/core/conf"
)

func fedTestCfg(t *testing.T, enabled bool) conf.FederationConfig {
	t.Helper()
	return conf.FederationConfig{
		Enabled:     enabled,
		RegistryURL: "http://127.0.0.1:1", // 不可达：单元测试不断言网络，只断言装配状态机
		PublicURL:   "http://127.0.0.1:2", // NewService 强制非空（公告给取件方的本站可达地址）
		NodeKeyPath: filepath.Join(t.TempDir(), "node.key"),
	}
}

// TestFederationRuntimeSwitch 状态机：启用→装配+attach；同配置重配→换新实例（先立后破）；
// 新配置装配失败→维持原状（回滚语义，不留半态）；关闭→stop+detach。
func TestFederationRuntimeSwitch(t *testing.T) {
	conf.SetGlobalConfig(&conf.AppConfiguration{}) // standalone 缺省
	t.Cleanup(func() { conf.SetGlobalConfig(&conf.AppConfiguration{}) })

	r := &federationRuntime{}
	attached, detached := 0, 0
	r.init("t", federationApp.NewService,
		func(s *federationApp.Service) { attached++ },
		func() { detached++ },
	)

	r.apply(fedTestCfg(t, true))
	if r.instance() == nil || attached != 1 {
		t.Fatalf("启用后应有实例并 attach: svc=%v attached=%d", r.instance() != nil, attached)
	}
	first := r.instance()

	// 同配置重配：新实例替换旧实例（先立后破），attach 再次触发
	r.apply(fedTestCfg(t, true))
	if r.instance() == nil || r.instance() == first || attached != 2 {
		t.Fatalf("重配应换新实例: same=%v attached=%d", r.instance() == first, attached)
	}

	// 装配失败：维持旧实例（回滚）
	keep := r.instance()
	rWithFail := &federationRuntime{
		start:   func(cfg conf.FederationConfig, _ string) (*federationApp.Service, error) { return nil, errors.New("boom") },
		current: keep,
		attach:  r.attach,
		detach:  r.detach,
	}
	rWithFail.apply(fedTestCfg(t, true))
	if rWithFail.instance() != keep {
		t.Fatal("装配失败应维持旧实例")
	}

	// 多副本部署：启用被拒并清空
	conf.SetGlobalConfig(&conf.AppConfiguration{Deployment: conf.DeploymentConfig{Mode: "public"}})
	r.apply(fedTestCfg(t, true))
	if r.instance() != nil {
		t.Fatal("多副本部署应拒绝 federation")
	}
	conf.SetGlobalConfig(&conf.AppConfiguration{})

	// 关闭：stop + detach
	r.apply(fedTestCfg(t, false))
	if r.instance() != nil || detached == 0 {
		t.Fatalf("关闭后应清空并 detach: svc=%v detached=%d", r.instance() != nil, detached)
	}
	r.stop() // 幂等
}
