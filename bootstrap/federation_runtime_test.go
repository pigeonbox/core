package bootstrap

import (
	"errors"
	"path/filepath"
	"sync"
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
		func() bool { return conf.GetGlobalConfig().IsStandalone() }, // 测试注入（生产由 wiring 固化）
	)

	same := fedTestCfg(t, true)
	if err := r.apply(same); err != nil {
		t.Fatalf("启用应成功: %v", err)
	}
	if r.instance() == nil || attached != 1 {
		t.Fatalf("启用后应有实例并 attach: svc=%v attached=%d", r.instance() != nil, attached)
	}
	first := r.instance()

	// 同配置重复保存：短路，不换实例不重挂（评审 P2-3 防抖）
	if err := r.apply(same); err != nil {
		t.Fatalf("同配置重配应成功: %v", err)
	}
	if r.instance() != first || attached != 1 {
		t.Fatalf("同配置应短路: newInstance=%v attached=%d", r.instance() != first, attached)
	}

	// 不同配置重配（NodeKeyPath 每次不同）：先立后破，换新实例
	if err := r.apply(fedTestCfg(t, true)); err != nil {
		t.Fatalf("换配置重配应成功: %v", err)
	}
	if r.instance() == nil || r.instance() == first || attached != 2 {
		t.Fatalf("重配应换新实例: same=%v attached=%d", r.instance() == first, attached)
	}

	// 装配失败：返回 error 且维持旧实例（回滚）
	keep := r.instance()
	rWithFail := &federationRuntime{
		start: func(cfg conf.FederationConfig, _ string) (*federationApp.Service, error) {
			return nil, errors.New("boom")
		},
		current: keep,
		attach:  r.attach,
		detach:  r.detach,
	}
	if err := rWithFail.apply(fedTestCfg(t, true)); err == nil {
		t.Fatal("装配失败应返回 error（透传给管理员）")
	}
	if rWithFail.instance() != keep {
		t.Fatal("装配失败应维持旧实例")
	}

	// 多副本部署：启用被拒——返回 error 且清空（管理员必须看到失败）
	conf.SetGlobalConfig(&conf.AppConfiguration{Deployment: conf.DeploymentConfig{Mode: "public"}})
	if err := r.apply(fedTestCfg(t, true)); err == nil {
		t.Fatal("多副本部署应返回拒绝 error")
	}
	if r.instance() != nil {
		t.Fatal("多副本部署应清空 federation")
	}
	conf.SetGlobalConfig(&conf.AppConfiguration{})

	// 关闭：stop + detach
	if err := r.apply(fedTestCfg(t, false)); err != nil {
		t.Fatalf("关闭应成功: %v", err)
	}
	if r.instance() != nil || detached == 0 {
		t.Fatalf("关闭后应清空并 detach: svc=%v detached=%d", r.instance() != nil, detached)
	}
	r.stop() // 幂等
}

// TestFederationRuntimeRegistryDefault 官方缺省（评审 P0-1）：registry 留空
// = 官方公共注册中心；public_url 留空且无法推断 → 响亮拒绝（不静默）。
func TestFederationRuntimeRegistryDefault(t *testing.T) {
	conf.SetGlobalConfig(&conf.AppConfiguration{}) // standalone
	t.Cleanup(func() { conf.SetGlobalConfig(&conf.AppConfiguration{}) })

	var gotRegistry string
	r := &federationRuntime{}
	r.init("t",
		func(cfg conf.FederationConfig, _ string) (*federationApp.Service, error) {
			gotRegistry = cfg.RegistryURL
			return federationApp.NewService(cfg, "t")
		},
		func(*federationApp.Service) {}, func() {},
		func() bool { return true },
	)
	// registry 留空 → 填官方缺省（cfg.PublicURL 显式给，避免推断分支）
	cfg := fedTestCfg(t, true)
	cfg.RegistryURL = ""
	if err := r.apply(cfg); err != nil {
		t.Fatalf("registry 留空应走官方缺省而非报错: %v", err)
	}
	if gotRegistry != defaultPublicRegistry {
		t.Fatalf("registry 缺省应为官方公共注册中心: got %q", gotRegistry)
	}
	r.stop()

	// public_url 留空 + 全局 conf 无 base_url（推断为空）→ 拒绝且报错可读
	cfg2 := fedTestCfg(t, true)
	cfg2.PublicURL = ""
	if err := r.apply(cfg2); err == nil {
		t.Fatal("public_url 无法推断时应响亮拒绝")
	}
}

// TestFederationRuntimeConcurrentApply 并发 apply 由 mutex 串行化，
// -race 下无数据竞争；任意交错后收敛到显式指定的终态。
func TestFederationRuntimeConcurrentApply(t *testing.T) {
	conf.SetGlobalConfig(&conf.AppConfiguration{})
	t.Cleanup(func() { conf.SetGlobalConfig(&conf.AppConfiguration{}) })

	r := &federationRuntime{}
	r.init("t", federationApp.NewService,
		func(*federationApp.Service) {},
		func() {},
		func() bool { return true },
	)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(on bool) {
			defer wg.Done()
			// 交错期间 enabled 分支可能因 standalone 门/装配失败报错，
			// 本用例只断言无数据竞争与终态收敛，错误本身不作断言
			_ = r.apply(fedTestCfg(t, on))
		}(i%2 == 0)
	}
	wg.Wait()
	if err := r.apply(fedTestCfg(t, false)); err != nil { // 收敛到关闭
		t.Fatalf("关闭应成功: %v", err)
	}
	if r.instance() != nil {
		t.Fatal("交错后显式关闭应收敛到无实例")
	}
}
