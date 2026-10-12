// config.go 站点配置的门面（facade）：类型别名 + 委派方法。
//
// 2026-10-10 配置中心独立为 app/config 域（全站配置的唯一写者），本文件只留
// admin 管理面 API 的委派层——gen/handler/admin、gen/handler/setup、
// transport admin_manage 等既有消费方零改动。新代码请直接消费 app/config
// （装配层先例：bootstrap 已直指 config.Default()）。
package admin

import (
	"context"

	appconfig "github.com/pigeonbox/core/app/config"
	"github.com/pigeonbox/core/conf"
)

// 类型别名：管理面 handler 构造/消费的类型不变（JSON 形状同源于 config 域）。
type (
	SystemConfig     = appconfig.SystemConfig
	UserSettings     = appconfig.UserSettings
	ReconfigureHooks = appconfig.ReconfigureHooks
	BaseConfig       = appconfig.BaseConfig
	StorageSettings  = appconfig.StorageSettings
	TransferSettings = appconfig.TransferSettings
)

// ConfigService 暴露内部配置中心实例（bootstrap 装配/守卫测试用；
// 管理面 handler 请走下方委派方法）。
func (s *Service) ConfigService() *appconfig.Service { return s.cfg }

// GetConfig 获取系统配置（委派 config 域；首次调用从 DB 懒加载）。
func (s *Service) GetConfig(ctx context.Context) (*SystemConfig, error) {
	return s.cfg.GetConfig(ctx)
}

// UpdateConfig 更新系统配置（委派 config 域；写穿 DB + 全局 conf overlay + 热重建钩子）。
func (s *Service) UpdateConfig(ctx context.Context, newConfig *SystemConfig) error {
	return s.cfg.UpdateConfig(ctx, newConfig)
}

// UpdateUserSettings 在线更新"用户配置"段（委派 config 域）。
func (s *Service) UpdateUserSettings(ctx context.Context, u UserSettings) error {
	return s.cfg.UpdateUserSettings(ctx, u)
}

// LoadRuntimeStorage 读取持久化的运行时存储配置（storage.RuntimePersister 实现，委派）。
func (s *Service) LoadRuntimeStorage(ctx context.Context) *conf.StorageConfig {
	return s.cfg.LoadRuntimeStorage(ctx)
}

// SaveRuntimeStorage 持久化运行时存储配置（storage.RuntimePersister 实现，委派）。
func (s *Service) SaveRuntimeStorage(ctx context.Context, cfg *conf.StorageConfig) error {
	return s.cfg.SaveRuntimeStorage(ctx, cfg)
}

// RestoreAdminSettings 启动时把持久化配置 overlay 回全局 conf（委派）。
func (s *Service) RestoreAdminSettings() { s.cfg.RestoreAdminSettings() }

// SetConfig 设置配置（显式注入优先于 DB 持久化配置；委派）。
func (s *Service) SetConfig(config *SystemConfig) { s.cfg.SetConfig(config) }

// SetReconfigureHooks 注册组件热重建钩子（bootstrap 调用；委派）。
func (s *Service) SetReconfigureHooks(h *ReconfigureHooks) { s.cfg.SetReconfigureHooks(h) }

// SetOnConfigPersisted 注入配置持久化广播回调（多副本模式；委派）。
func (s *Service) SetOnConfigPersisted(fn func()) { s.cfg.SetOnConfigPersisted(fn) }

// InvalidateRuntimeConfig 丢弃内存配置缓存（多副本广播接收侧；委派）。
func (s *Service) InvalidateRuntimeConfig() { s.cfg.InvalidateRuntimeConfig() }

// EffectiveUserSettings 生效的用户设置（委派 config 域包级入口：
// 持久化值优先，无记录回退 yaml）。
func EffectiveUserSettings(ctx context.Context) UserSettings {
	return appconfig.EffectiveUserSettings(ctx)
}
