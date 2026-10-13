package bootstrap

// P1（2026-10-14 对称节点 spec §6）：federation 装配收口为可重入运行时——
// 启动种子（env/yaml；RestoreAdminSettings 已 overlay DB 值）与 admin 后台
// 热切换走同一条路径。先立后破：新实例装配成功才停旧；失败维持原状（回滚）。
// 多副本硬约束运行时化：节点身份是进程级 Ed25519，standalone 专属。

import (
	"sync"

	"go.uber.org/zap"

	federationApp "github.com/pigeonbox/core/app/federation"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/logger"
)

// fedStartFunc 生产工厂 = federationApp.NewService；测试注入可控工厂。
type fedStartFunc func(cfg conf.FederationConfig, nodeName string) (*federationApp.Service, error)

type federationRuntime struct {
	mu         sync.Mutex
	start      fedStartFunc
	attach     func(*federationApp.Service) // 挂注入点（share/admin/customHandler/MCP）
	detach     func()                        // 摘注入点（全部置非类型化 nil——typed-nil 进接口非 nil 的历史坑）
	standalone func() bool                   // 部署形态注入（棘轮规则 8：不直读全局 conf）
	current    *federationApp.Service
	cfg        conf.FederationConfig
	nodeName   string
}

// fedRT 包级单例（initThriftIDLServices 装配；Cleanup 停机）。
var fedRT = &federationRuntime{}

// init 注入工厂与注入点（装配期调用一次）。
func (r *federationRuntime) init(nodeName string, start fedStartFunc,
	attach func(*federationApp.Service), detach func(), standalone func() bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nodeName, r.start, r.attach, r.detach, r.standalone = nodeName, start, attach, detach, standalone
}

// apply 按新配置切换 federation（启动路径与热路径共用；串行化）。
func (r *federationRuntime) apply(cfg conf.FederationConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg = cfg
	if !cfg.Enabled {
		r.stopLocked()
		return
	}
	if r.standalone != nil && !r.standalone() {
		logger.Warn("federation 启用被拒：多副本部署语义未定义（standalone 专属）")
		r.stopLocked()
		return
	}
	svc, err := r.start(cfg, r.nodeName)
	if err != nil {
		// 回滚语义：新配置装配失败维持原状，不留半态
		logger.Error("federation 新配置装配失败，维持原配置", zap.Error(err))
		return
	}
	if r.current != nil {
		r.current.Stop()
		if r.detach != nil {
			r.detach()
		}
	}
	r.current = svc
	svc.Start()
	if r.attach != nil {
		r.attach(svc)
	}
	logger.Info("federation enabled",
		zap.String("registry", cfg.RegistryURL),
		zap.String("public_url", cfg.PublicURL),
		zap.String("node_id", svc.NodeID()))
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
func (r *federationRuntime) instance() *federationApp.Service {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current
}
