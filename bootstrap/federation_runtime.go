package bootstrap

// P1（2026-10-14 对称节点 spec §6）：federation 装配收口为可重入运行时——
// 启动种子（env/yaml；RestoreAdminSettings 已 overlay DB 值）与 admin 后台
// 热切换走同一条路径。先立后破：新实例装配成功才停旧；失败维持原状（回滚）。
// 多副本硬约束运行时化：节点身份是进程级 Ed25519，standalone 专属。

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"go.uber.org/zap"

	federationApp "github.com/pigeonbox/core/app/federation"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/logger"
)

// defaultPublicRegistry 官方公共注册中心（spec 2026-10-14 §3.1/§5：registry
// 留空 = 官方缺省；自建 p2pd 时显式配置）。信令 56788，中继经 /health 自发现。
const defaultPublicRegistry = "http://home.enjoye.top:56788"

// fedStartFunc 生产工厂 = federationApp.NewNode；测试注入可控工厂。
type fedStartFunc func(cfg conf.FederationConfig, nodeName string) (*federationApp.Node, error)

type federationRuntime struct {
	mu         sync.Mutex
	start      fedStartFunc
	attach     func(*federationApp.Node) // 挂注入点（share/admin/customHandler/MCP）
	detach     func()                    // 摘注入点（全部置非类型化 nil——typed-nil 进接口非 nil 的历史坑）
	standalone func() bool               // 部署形态注入（棘轮规则 8：不直读全局 conf）
	current    *federationApp.Node
	cfg        conf.FederationConfig
	nodeName   string
}

// fedRT 包级单例（initThriftIDLServices 装配；Cleanup 停机）。
var fedRT = &federationRuntime{}

// init 注入工厂与注入点（装配期调用一次）。
// 注意：attach/detach 在 apply 持锁期间执行——闭包内严禁回调 fedRT 的
// instance()/apply()/stop()（同锁重入即死锁）；闭包内只允许对注入目标
// 做 setter 调用（这些 setter 自身已按热切换并发安全加锁）。
func (r *federationRuntime) init(nodeName string, start fedStartFunc,
	attach func(*federationApp.Node), detach func(), standalone func() bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nodeName, r.start, r.attach, r.detach, r.standalone = nodeName, start, attach, detach, standalone
}

// apply 按新配置切换 federation（启动路径与热路径共用；串行化）。
// 返回 error 表示"本次切换未生效（联邦维持原状/未启用）"——配置层可能已
// 持久化新值，调用方（UpdateConfig 钩子）必须把错误透传给管理员，
// 禁止静默（spec §12.4 断链响亮；评审 P0/P1-1：曾因吞错致"保存成功但联邦从未启动"）。
// 失败路径一律维持原实例（回滚语义，不留半态）。
func (r *federationRuntime) apply(cfg conf.FederationConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !cfg.Enabled {
		r.cfg = cfg
		r.stopLocked()
		return nil
	}
	if r.standalone != nil && !r.standalone() {
		r.cfg = cfg
		r.stopLocked()
		return errors.New("多副本部署禁用联邦（standalone 专属；请用单机 standalone 模式或保持关闭）")
	}
	// spec §3.1/§5 缺省语义（评审 P0-1：界面提示"留空=官方公共注册中心"，
	// 后端却曾对空值报错——主路径静默失败）：
	//   registry_url 留空 → 官方公共注册中心
	eff := cfg
	if strings.TrimSpace(eff.RegistryURL) == "" {
		eff.RegistryURL = defaultPublicRegistry
	}
	// public_url 留空 → 站点对外地址（server.base_url）；推断不出或监听
	// 0.0.0.0 时响亮拒绝——拼进公告即不可达取件链接（2026-10-07 iStoreOS 事故同款）
	if strings.TrimSpace(eff.PublicURL) == "" && config != nil {
		eff.PublicURL = bootstrapBaseURL()
	}
	if eff.PublicURL == "" || strings.Contains(eff.PublicURL, "0.0.0.0") {
		return fmt.Errorf("public_url 为空且无法从站点配置推断可用对外地址（当前 %q），请显式填写本站对外地址", eff.PublicURL)
	}
	// 同配置重复保存：短路，不触发 deregister/register 抖动（评审 P2-3）
	if r.current != nil && cfg == r.cfg {
		return nil
	}
	svc, err := r.start(eff, r.nodeName)
	if err != nil {
		// 回滚语义：新配置装配失败维持原状，不留半态
		return fmt.Errorf("federation 装配失败（维持原配置）: %w", err)
	}
	if r.current != nil {
		r.current.Stop()
		if r.detach != nil {
			r.detach()
		}
	}
	r.current = svc
	r.cfg = cfg
	svc.Start()
	if r.attach != nil {
		r.attach(svc)
	}
	logger.Info("federation enabled",
		zap.String("registry", eff.RegistryURL),
		zap.String("public_url", eff.PublicURL),
		zap.String("node_id", svc.NodeID()))
	return nil
}

// stopLocked 停并摘注入点（调用方持锁）。
func (r *federationRuntime) stopLocked() {
	if r.current != nil {
		r.current.Stop()
		r.current = nil
	}
	if r.detach != nil {
		r.detach()
	}
}

// stop 停机（Cleanup 调用；幂等）。
func (r *federationRuntime) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopLocked()
}

// instance 当前联邦实例（nil=未启用）。
func (r *federationRuntime) instance() *federationApp.Node {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current
}
