// Package config 全站配置中心域：system_configs 单行 JSON 的唯一写者。
//
// 2026-10-10 自 admin 域独立——此前三段职责挤在 admin：管理面 API、治理操作
// （用户/文件/统计/维护）、以及被 user/storage/bootstrap/mcp 四面消费的
// 全站配置中心（~500 行）。配置中心的归属错位使 admin 成为事实上的上帝域，
// 且"管理端在线设置"的读写方横跨四个域。独立后：
//   - 本域持有：内存缓存+懒加载、写穿 DB、校验、全局 conf overlay 热应用、
//     组件热重建钩子（ReconfigureHooks）、多副本变更广播回调、运行时存储
//     配置持久化（storage.RuntimePersister 实现）、用户设置生效语义
//     （EffectiveUserSettings）与配置操作审计。
//   - admin 域保留管理面 API 门面（委派方法 + 类型别名，消费方零改动）与
//     治理/审计职责。
//   - bootstrap（装配层）直接指向本域单例接线。
//
// 单实例约束：system_configs 是"单行 JSON 读改写"语义，必须全站单实例——
// 多实例各自缓存内存副本会互相覆盖（用户设置写入后公共配置读旧值的
// 2026-10-03 事故即由此而来）。Default() 是全站唯一实例；NewService()
// 仅供测试/嵌入式构造独立实例。
package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/auth"
	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// Service 配置中心服务。
type Service struct {
	configRepo         *dao.SystemConfigRepository
	adminOperationRepo *dao.AdminOperationLogRepository // 配置操作审计（与 admin 域同表）

	configMu     sync.RWMutex
	config       *SystemConfig
	configLoaded bool // 已尝试过 DB 加载（含无记录的情况），避免每次读都打 DB

	hooksMu       sync.RWMutex
	reconfigurers *ReconfigureHooks
	onPersist     func() // 配置持久化回调（bootstrap 注入；多副本 admin 模式发布变更广播）
}

// NewService 构造独立实例（测试/嵌入式；全站运行时请用 Default()——
// 单行读改写语义不允许多实例并存，见包注释）。
func NewService(gormDB *gorm.DB) *Service {
	return &Service{
		configRepo:         dao.NewSystemConfigRepository(gormDB),
		adminOperationRepo: dao.NewAdminOperationLogRepository(gormDB),
		// config 保持 nil，由 GetConfig 懒加载默认值 → DB 持久化配置
	}
}

// defaultSvc 包级单例（InitDefault 显式初始化；单实例约束见包注释）。
var (
	defaultMu  sync.RWMutex
	defaultSvc *Service
)

// InitDefault 注册全站配置中心单例（bootstrap 在 InitDatabase 后调用；幂等）。
// 单实例约束见包注释：system_configs 单行读改写不允许多实例并存。
func InitDefault(gormDB *gorm.DB) {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defaultSvc == nil {
		defaultSvc = NewService(gormDB)
	}
}

// Default 全站共享的配置中心实例。未 InitDefault 直接取用是装配错误，fail-loud。
func Default() *Service {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	if defaultSvc == nil {
		panic("config: Default() before InitDefault——bootstrap 必须在 InitDatabase 后调 InitDefault")
	}
	return defaultSvc
}

// EffectiveUserSettings 生效的用户设置：优先管理后台持久化值，
// 无记录时回退 yaml 全局配置。注册开关、配额默认值、会话时长的
// 运行时消费点统一走这里（修复管理端开关不生效）。
func EffectiveUserSettings(ctx context.Context) UserSettings {
	cfg, err := Default().GetConfig(ctx)
	if err != nil || cfg == nil || cfg.User == nil {
		return *userSettingsFromYAML()
	}
	return *cfg.User
}

// ReconfigureHooks 存储域之外的组件热重建钩子（bootstrap 注册；SMTP mailer / OIDC service）。
// 参数为合并后的新段；nil 段表示维持现状。
type ReconfigureHooks struct {
	// OnNotifyChanged Webhook+SMTP 热重建（mailer 按新 SMTP 段重建，host 空=卸载）
	OnNotifyChanged func(n *conf.NotifyConfig)
	// OnOIDCChanged OIDC service 热重建（按新段；Enabled=false 时 handler 走未启用响应）
	OnOIDCChanged func(o *conf.OIDCConfig)
	// OnP2PFederationChanged federation 运行时热切换（bootstrap 注册；P1 对称节点
	// spec §6）。返回 error 时 UpdateConfig 透传给管理员——切换失败但配置已持久化
	// 的分叉必须响亮（spec §12.4），禁止静默。
	OnP2PFederationChanged func(f *conf.FederationConfig) error
	// OnOverlayApplied SystemConfig 段 overlay 进全局 conf 之后的回调（bootstrap
	// 注册 env 恒覆盖重施器：统一优先级 yaml < DB < env，2026-10-15）。持 configMu
	// 同步调用，实现方只许操作全局 conf/env，不得回调 config service。
	OnOverlayApplied func()
}

// SetReconfigureHooks 注册组件热重建钩子（bootstrap 调用）。
func (s *Service) SetReconfigureHooks(h *ReconfigureHooks) {
	s.hooksMu.Lock()
	defer s.hooksMu.Unlock()
	s.reconfigurers = h
}

// SetOnConfigPersisted 注入配置持久化回调（bootstrap 调用）。多副本拆分时
// admin 实例经它发布 Redis 变更广播，public 副本订阅后失效本地缓存并热重建
// （详见 docs/specs/2026-10-06-multi-replica-deployment-modes.md §4）。
func (s *Service) SetOnConfigPersisted(fn func()) {
	s.hooksMu.Lock()
	defer s.hooksMu.Unlock()
	s.onPersist = fn
}

// notifyConfigPersisted 配置已持久化（UpdateConfig / SaveRuntimeStorage 成功路径末尾调用）。
func (s *Service) notifyConfigPersisted() {
	s.hooksMu.RLock()
	fn := s.onPersist
	s.hooksMu.RUnlock()
	if fn != nil {
		fn()
	}
}

// SetConfig 设置配置（显式注入优先于 DB 持久化配置）
func (s *Service) SetConfig(config *SystemConfig) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	s.config = config
	s.configLoaded = true
}

// InvalidateRuntimeConfig 丢弃内存配置缓存（仅影响缓存，不写库）。
// 多副本：public 副本收到管理端变更广播后调用，下次 GetConfig 重读 DB——
// 单机 standalone 不调用（保持既有"进程内即真相"语义）。
func (s *Service) InvalidateRuntimeConfig() {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	s.config = nil
	s.configLoaded = false
}

// loadPersistedConfig 从 DB 读取持久化配置（单行 system_configs）。
// 无记录返回 (nil, nil)；DB 不可用/记录损坏仅告警并回退默认值——
// 配置读取失败不应阻断业务启动。
func (s *Service) loadPersistedConfig(ctx context.Context) (*SystemConfig, error) {
	rec, err := s.configRepo.Get(ctx)
	if err != nil {
		if errors.Is(err, dao.ErrDBNotInitialized) {
			return nil, nil
		}
		return nil, err
	}
	if rec == nil {
		return nil, nil
	}
	cfg := defaultSystemConfig()
	if err := json.Unmarshal([]byte(rec.Data), cfg); err != nil {
		logger.Error("system config record corrupted, fallback to defaults",
			zap.String("data", rec.Data), zap.Error(err))
		return nil, err
	}
	// 历史记录没有 user 段：unmarshal 后是 nil/零值，回退 yaml，
	// 否则升级后会意外把注册当成"关闭"（2026-10-03 假开关接线时补）
	if cfg.User == nil {
		cfg.User = userSettingsFromYAML()
	}
	return cfg, nil
}

// ensureConfigLoaded 懒加载：首次访问时用 DB 持久化配置覆盖默认值。
func (s *Service) ensureConfigLoaded(ctx context.Context) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if s.config == nil {
		s.config = defaultSystemConfig()
	}
	if s.configLoaded {
		return
	}
	s.configLoaded = true
	persisted, err := s.loadPersistedConfig(ctx)
	switch {
	case err != nil:
		logger.Error("failed to load persisted system config, using defaults", zap.Error(err))
	case persisted != nil:
		s.config = persisted
		siteName := ""
		if persisted.Base != nil {
			siteName = persisted.Base.Name
		}
		logger.Info("system config loaded from database",
			zap.String("site_name", siteName))
	}
	// UploadEx 从未在线保存过时回填全局 conf 生效值：管理后台能看到真实
	// 生效的上传准入配置，整段保存也不会把 yaml 值冲成零值
	if s.config.UploadEx == nil {
		if g := conf.GetGlobalConfig(); g != nil {
			ue := g.Upload
			s.config.UploadEx = &ue
		}
	}
	// P2P 段回填在 applySystemConfigOverlayLocked 内做（复用其全局 conf 读点，
	// 棘轮规则 8：不新增 GetGlobalConfig 直读；语义等价——启动 RestoreAdminSettings
	// 与每次 UpdateConfig 都会经过 overlay）
	s.applyUserSideEffects(s.config.User)
}

// applyUserSideEffects 用户设置中需要"生效"而非仅存储的部分：
// 会话时长写入 auth 包（新签发 token 即刻采用）。
func (s *Service) applyUserSideEffects(u *UserSettings) {
	if u == nil {
		return
	}
	if u.SessionExpiryHours > 0 {
		auth.SetSessionExpiry(time.Duration(u.SessionExpiryHours) * time.Hour)
	}
}

// GetConfig 获取系统配置（首次调用会从 DB 加载管理后台保存的配置）
func (s *Service) GetConfig(ctx context.Context) (*SystemConfig, error) {
	s.ensureConfigLoaded(ctx)
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.config, nil
}

// UpdateConfig 更新系统配置：写穿到 DB（单行），成功后才更新内存。
// 防御性合并：handler 会从请求重建 SystemConfig，请求不含 runtime_storage
// （存储域维护的运行时段），为 nil 时保留旧值，避免保存站点配置时被冲掉。
func (s *Service) UpdateConfig(ctx context.Context, newConfig *SystemConfig) error {
	if err := validateSystemConfig(newConfig); err != nil {
		return err
	}
	// 提交意图先于 nil-保留合并捕获：热切换钩子只在请求真正携带该段时触发。
	// overlay 回填（见 applySystemConfigOverlayLocked）会让内存里的 P2P 段恒非
	// nil（env 种子镜像），若以合并后状态判空，任何局部保存都会误触发
	// federation stop/start 抖动。
	p2pFedSubmitted := newConfig.P2P != nil && newConfig.P2P.Federation != nil
	s.ensureConfigLoaded(ctx)
	s.configMu.RLock()
	if newConfig.RuntimeStorage == nil && s.config != nil {
		newConfig.RuntimeStorage = s.config.RuntimeStorage
	}
	// 通用配置保存（thrift 通道）不带 user 段，保留旧值避免被冲掉
	if newConfig.User == nil && s.config != nil {
		newConfig.User = s.config.User
	}
	// 值类型段同规则（2026-10-12 修复：此前 base/storage/transfer 为匿名值类型、
	// 不在保留清单内，任何局部保存都会把它们按零值写库——站点名/端口/传输默认值被清空）
	if newConfig.Base == nil && s.config != nil {
		newConfig.Base = s.config.Base
	}
	if newConfig.Storage == nil && s.config != nil {
		newConfig.Storage = s.config.Storage
	}
	if newConfig.Transfer == nil && s.config != nil {
		newConfig.Transfer = s.config.Transfer
	}
	// v0.7.3 扩容段同规则：请求未携带的段保留旧值（前端按 tab 分批提交）
	if newConfig.UI == nil && s.config != nil {
		newConfig.UI = s.config.UI
	}
	if newConfig.UploadEx == nil && s.config != nil {
		newConfig.UploadEx = s.config.UploadEx
	}
	if newConfig.Download == nil && s.config != nil {
		newConfig.Download = s.config.Download
	}
	if newConfig.Notify == nil && s.config != nil {
		newConfig.Notify = s.config.Notify
	}
	if newConfig.LocalImport == nil && s.config != nil {
		newConfig.LocalImport = s.config.LocalImport
	}
	if newConfig.OIDC == nil && s.config != nil {
		newConfig.OIDC = s.config.OIDC
	}
	if newConfig.APIToken == nil && s.config != nil {
		newConfig.APIToken = s.config.APIToken
	}
	// P2P 段同规则（2026-10-14 对称节点 P1：局部保存不冲掉联邦配置）。
	// 子段 nil 保留下沉一层（联邦/直传同挂 p2p 下，互不冲——P2a 起）。
	if newConfig.P2P == nil && s.config != nil {
		newConfig.P2P = s.config.P2P
	} else if newConfig.P2P != nil && s.config != nil && s.config.P2P != nil {
		if newConfig.P2P.Federation == nil {
			newConfig.P2P.Federation = s.config.P2P.Federation
		}
		if newConfig.P2P.Direct == nil {
			newConfig.P2P.Direct = s.config.P2P.Direct
		}
	}
	s.configMu.RUnlock()
	data, err := json.Marshal(newConfig)
	if err != nil {
		return err
	}
	rec := &model.SystemConfigRecord{Data: string(data)}
	if err := s.configRepo.Save(ctx, rec); err != nil {
		if errors.Is(err, dao.ErrDBNotInitialized) {
			// DB 不可用时仅更新内存（纯内存运行形态，如部分单测）
			logger.CtxWarn(ctx, "system config not persisted: database not initialized")
		} else {
			return err
		}
	}
	s.applyUserSideEffects(newConfig.User)
	s.configMu.Lock()
	s.config = newConfig
	s.configLoaded = true
	// 新段热应用：整体 overlay 到全局 conf（所有 conf.GetGlobalConfig() 读取点即刻生效）
	s.applySystemConfigOverlayLocked(newConfig)
	notifyCfg, oidcCfg := newConfig.Notify, newConfig.OIDC
	var p2pFedCfg *conf.FederationConfig
	if newConfig.P2P != nil {
		p2pFedCfg = newConfig.P2P.Federation
	}
	s.configMu.Unlock()

	// 组件热重建（bootstrap 注册的钩子；nil 段不触发）
	s.hooksMu.RLock()
	hooks := s.reconfigurers
	s.hooksMu.RUnlock()
	if hooks != nil {
		if notifyCfg != nil && hooks.OnNotifyChanged != nil {
			hooks.OnNotifyChanged(notifyCfg)
		}
		if oidcCfg != nil && hooks.OnOIDCChanged != nil {
			hooks.OnOIDCChanged(oidcCfg)
		}
		if p2pFedSubmitted && p2pFedCfg != nil && hooks.OnP2PFederationChanged != nil {
			if err := hooks.OnP2PFederationChanged(p2pFedCfg); err != nil {
				// 配置已写库/overlay，但运行态切换失败（维持原状）——审计记失败
				// 并把错误透传给管理员，禁止"保存成功但联邦从未启动"式静默
				s.logConfigOperation(ctx, "config.update",
					"p2p.federation apply failed: "+err.Error(), false)
				return fmt.Errorf("p2p.federation 配置已保存但未生效: %w", err)
			}
		}
	}

	s.logConfigOperation(ctx, "config.update", "system config persisted to database", true)
	// 广播变更（多副本 admin 模式；standalone 下回调为 nil 空转）
	s.notifyConfigPersisted()
	return nil
}

// applySystemConfigOverlayLocked 把新段 overlay 到全局 conf（调用方持 configMu）。
// 全局 conf 是几乎所有读取点（utils/gate/handler/middleware）的真相源，
// 原地改写即全站热生效；重启后由 RestoreAdminSettings 从 system_configs 恢复。
func (s *Service) applySystemConfigOverlayLocked(sc *SystemConfig) {
	g := conf.GetGlobalConfig()
	if g == nil {
		return
	}
	if sc.UI != nil {
		g.UI = *sc.UI
	}
	if sc.UploadEx != nil {
		// 保留既有 local_import（2026-10-05 审计附带 bug：UploadEx 整段覆盖
		// 会把零值的 LocalImport 一并写入——"上传"tab 保存未携带该段时
		// 冲掉已配置 roots）
		prevLocal := g.Upload.LocalImport
		g.Upload = *sc.UploadEx
		if sc.LocalImport == nil && !sc.UploadEx.LocalImport.Enabled && len(sc.UploadEx.LocalImport.Roots) == 0 {
			g.Upload.LocalImport = prevLocal
		}
	}
	if sc.Download != nil {
		g.Download = *sc.Download
	}
	if sc.Notify != nil {
		g.Notify = *sc.Notify
	}
	if sc.LocalImport != nil {
		g.Upload.LocalImport = *sc.LocalImport
	}
	if sc.OIDC != nil {
		g.Security.OIDC = *sc.OIDC
	}
	if sc.APIToken != nil {
		g.Security.APIToken = *sc.APIToken
	}
	// P2P 段从未在线保存过时回填全局 conf 生效值（与 UploadEx 回填同理：管理
	// 后台看到真实生效的联邦配置，整段保存也不会把 env 种子冲成零值）。注意
	// 语义：回填本身不写库，但随后任意一次保存经 nil-保留会把镜像值持久化
	// ——与 UploadEx 先例同语义（env 种子自此以 DB 值固化，后台可改）。g 已在
	// 本函数读取，不新增直读点（棘轮规则 8）。
	if sc.P2P == nil {
		fed := g.Federation
		sc.P2P = &P2PSection{Federation: &fed}
	}
	if sc.P2P.Federation != nil {
		g.Federation = *sc.P2P.Federation
	}
	// env 恒覆盖（统一优先级 yaml < DB < env，2026-10-15）：上面的 DB overlay 整段
	// 盖掉了启动时 viper 绑定的 env 值，bootstrap 注册的重施器在此恢复 env 语义
	// （storage 段在 restoreRuntimeStorage 已是同规则）。随后把生效值回镜像进
	// sc（仅内存；DB 保留提交意图），管理后台读到的恒为生效值。
	s.hooksMu.RLock()
	overlayApplied := s.reconfigurers
	s.hooksMu.RUnlock()
	if overlayApplied != nil && overlayApplied.OnOverlayApplied != nil {
		overlayApplied.OnOverlayApplied()
	}
	s.syncMirrorFromGlobalLocked(g, sc)
}

// syncMirrorFromGlobalLocked 把全局 conf 托管段回镜像进 sc（调用方持 configMu；
// 仅内存，不写库、不触发热重建）。env 恒覆盖后若内存镜像停留在提交值，
// 管理后台会展示"保存成功但没生效"的假象。g 复用 overlay 的直读（棘轮规则 8）。
func (s *Service) syncMirrorFromGlobalLocked(g *conf.AppConfiguration, sc *SystemConfig) {
	if g == nil {
		return
	}
	if sc.UI != nil {
		ui := g.UI
		sc.UI = &ui
	}
	if sc.UploadEx != nil {
		ue := g.Upload
		sc.UploadEx = &ue
	}
	if sc.LocalImport != nil {
		li := g.Upload.LocalImport
		sc.LocalImport = &li
	}
	if sc.Download != nil {
		d := g.Download
		sc.Download = &d
	}
	if sc.Notify != nil {
		n := g.Notify
		sc.Notify = &n
	}
	if sc.OIDC != nil {
		o := g.Security.OIDC
		sc.OIDC = &o
	}
	if sc.APIToken != nil {
		a := g.Security.APIToken
		sc.APIToken = &a
	}
	if sc.P2P != nil && sc.P2P.Federation != nil {
		f := g.Federation
		sc.P2P.Federation = &f
	}
}

// RestoreAdminSettings 启动时把持久化的新段 overlay 回全局 conf（DB 先于服务启动就绪）。
// 由 bootstrap 在 restoreRuntimeStorage 同相位调用。
func (s *Service) RestoreAdminSettings() {
	s.ensureConfigLoaded(context.Background())
	s.configMu.RLock()
	sc := s.config
	s.configMu.RUnlock()
	if sc == nil {
		return
	}
	s.applySystemConfigOverlayLocked(sc)
}

// UpdateUserSettings 在线更新"用户配置"段：读改写当前配置，仅替换 User，
// 其余段保持不变；写穿 DB 并即时应用副作用（会话时长）。
func (s *Service) UpdateUserSettings(ctx context.Context, u UserSettings) error {
	if err := validateUserSettings(&u); err != nil {
		return err
	}
	s.ensureConfigLoaded(ctx)
	s.configMu.RLock()
	current := s.config
	s.configMu.RUnlock()
	if current == nil {
		current = defaultSystemConfig()
	}
	// 深拷贝当前配置，只替换 User 段
	data, err := json.Marshal(current)
	if err != nil {
		return err
	}
	next := defaultSystemConfig()
	if err := json.Unmarshal(data, next); err != nil {
		return err
	}
	if next.User == nil {
		next.User = &UserSettings{}
	}
	*next.User = u
	return s.UpdateConfig(ctx, next)
}

// LoadRuntimeStorage 读取持久化的运行时存储配置（storage.RuntimePersister 实现）。
// 无记录/未设置返回 nil；DB 不可用返回 nil（与配置加载的降级策略一致）。
func (s *Service) LoadRuntimeStorage(ctx context.Context) *conf.StorageConfig {
	s.ensureConfigLoaded(ctx)
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	if s.config == nil {
		return nil
	}
	return s.config.RuntimeStorage
}

// SaveRuntimeStorage 持久化运行时存储配置（storage.RuntimePersister 实现）。
// 读改写单行 JSON，只更新 runtime_storage 段；DB 不可用时返回错误，
// 由调用方（存储域）决定提示语义——切换已生效，仅持久化失败。
func (s *Service) SaveRuntimeStorage(ctx context.Context, cfg *conf.StorageConfig) error {
	s.ensureConfigLoaded(ctx)
	s.configMu.Lock()
	s.config.RuntimeStorage = cfg
	data, err := json.Marshal(s.config)
	s.configMu.Unlock()
	if err != nil {
		return err
	}
	rec := &model.SystemConfigRecord{Data: string(data)}
	if err := s.configRepo.Save(ctx, rec); err != nil {
		if errors.Is(err, dao.ErrDBNotInitialized) {
			return fmt.Errorf("database not initialized: %w", err)
		}
		return err
	}
	s.logConfigOperation(ctx, "storage.config.persist", "runtime storage config persisted (type="+cfg.Type+")", true)
	// 广播变更（多副本 admin 模式；public 副本收到后对存储单例 Reload）
	s.notifyConfigPersisted()
	return nil
}

// logConfigOperation 写入配置操作审计（与 admin 域治理操作同表同语义；
// 操作者提取走 middleware.AuditActor 单源）。审计失败只记运行日志，不阻断业务。
func (s *Service) logConfigOperation(ctx context.Context, action, target string, success bool) {
	actorID, actorName, ip := middleware.AuditActor(ctx)
	entry := &model.AdminOperationLog{
		Action:    action,
		Target:    target,
		Success:   success,
		ActorID:   actorID,
		ActorName: actorName,
		IP:        ip,
	}
	if err := s.adminOperationRepo.Create(ctx, entry); err != nil {
		logger.CtxWarn(ctx, "config audit log write failed", "action", action, "err", err)
	}
}
