// config.go 站点配置模型与校验:SystemConfig/UserSettings 类型、默认值、
// 写库前校验(纯函数簇,自 admin 域原样迁入)。
package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/security"
)

// BaseConfig 站点基础信息段。指针语义与其余段一致：nil = 本次未提交，
// UpdateConfig 保留现状（值类型无法表达"未提交"，局部保存曾把整段清零）。
type BaseConfig struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Port        int    `json:"port"`
}

// StorageSettings 存储概览段（nil 语义同 BaseConfig；运行时存储后端走 runtime_storage）。
type StorageSettings struct {
	Type    string `json:"type"`
	MaxSize int64  `json:"max_size"`
}

// TransferSettings 分享传输默认值段（nil 语义同 BaseConfig）。
type TransferSettings struct {
	MaxCount      int `json:"max_count"`
	ExpireDefault int `json:"expire_default"`
}

type SystemConfig struct {
	Base     *BaseConfig       `json:"base,omitempty"`
	Storage  *StorageSettings  `json:"storage,omitempty"`
	Transfer *TransferSettings `json:"transfer,omitempty"`

	// RuntimeStorage 运行时存储配置（存储域经 RuntimePersister 接口读写，
	// 本域只负责持久化不解释其内容；nil 表示管理端从未在线改过存储后端）。
	RuntimeStorage *conf.StorageConfig `json:"runtime_storage,omitempty"`

	// User 用户设置（注册开关/上传限制/存储配额/会话时长）。
	// nil 表示从未在线保存过 → 运行时回退 yaml 全局配置（EffectiveUserSettings）。
	// JSON 键沿用前端 configForm.user 的既有键名，读写端点直传免转换。
	User *UserSettings `json:"user,omitempty"`

	// ===== v0.7.3 扩容的在线设置段（同 User 语义：nil = 未在线设置 → yaml 生效；
	// 非 nil = 保存时整体 overlay 到全局 conf 并热应用，重启由 restoreAdminSettings 恢复）=====
	// 外观（背景图/主题色/管理入口可见性）
	UI *conf.UIConfig `json:"ui,omitempty"`
	// 上传细项（文本上限/扩展名白黑名单/匿名日配额/整文件上限/全局过期上限/过期样式裁剪）
	UploadEx *conf.UploadConfig `json:"upload_ex,omitempty"`
	// 下载（S3 直下/超时/需登录）
	Download *conf.DownloadConfig `json:"download,omitempty"`
	// 通知（Webhook + SMTP；保存经 Reconfigurers 热重建 mailer）
	Notify *conf.NotifyConfig `json:"notify,omitempty"`
	// 本地导入（enabled/roots）
	LocalImport *conf.LocalImportConfig `json:"local_import,omitempty"`
	// OIDC 单点登录
	OIDC *conf.OIDCConfig `json:"oidc,omitempty"`
	// API Key 认证总开关与 per-Key 限流
	APIToken *conf.APITokenConfig `json:"api_token,omitempty"`

	// P2P P2P 能力段（2026-10-14 对称节点 spec §5）：Federation 控制面在线开关；
	// 直传数据面参数（enabled/send_roots/…）随 P2a 落地。nil = 未在线设置。
	P2P *P2PSection `json:"p2p,omitempty"`

	// 新段是否发生变化的标记由「非 nil」即涵盖；保存走 UpdateConfig 既有持久化通道
}

// UserSettings 管理后台"用户配置"标签页的在线设置。
// 语义：作为对应运行时消费点的系统级默认值（用户级覆盖仍然优先）。
type UserSettings struct {
	// AllowUserRegistration 是否开放注册（/user/register 与 /api/config 同源读取）
	AllowUserRegistration bool `json:"allowuserregistration"`
	// UserUploadSize 用户单次上传大小默认上限（字节，0 = 不限）
	UserUploadSize int64 `json:"useruploadsize"`
	// UserStorageQuota 用户存储配额默认值（字节，0 = 不限）
	UserStorageQuota int64 `json:"userstoragequota"`
	// SessionExpiryHours 会话时长（小时，0 = auth 包默认 7 天）
	SessionExpiryHours int `json:"sessionexpiryhours"`
}

// userSettingsFromYAML 无持久化记录时回退 yaml 全局配置（与历史行为一致）。
func userSettingsFromYAML() *UserSettings {
	u := &UserSettings{SessionExpiryHours: 168}
	if cfg := conf.GetGlobalConfig(); cfg != nil {
		u.AllowUserRegistration = cfg.User.AllowUserRegistration
		u.UserUploadSize = cfg.User.UserUploadSize
		u.UserStorageQuota = cfg.User.UserStorageQuota
		if cfg.User.SessionExpiryHours > 0 {
			u.SessionExpiryHours = cfg.User.SessionExpiryHours
		}
	}
	return u
}

// defaultSystemConfig 站点配置默认值。
func defaultSystemConfig() *SystemConfig {
	cfg := &SystemConfig{}
	cfg.Base = &BaseConfig{Name: "PigeonBox", Description: "文件分享平台", Port: 8888}
	cfg.Storage = &StorageSettings{Type: "local", MaxSize: 1024 * 1024 * 1024} // 1GB
	cfg.Transfer = &TransferSettings{MaxCount: 100, ExpireDefault: 7}         // 7天
	cfg.User = userSettingsFromYAML()
	return cfg
}

// validateSystemConfig 基础有效性校验（写库前的最后一道防线）。
func validateSystemConfig(cfg *SystemConfig) error {
	if cfg == nil {
		return errors.New("config is nil")
	}
	// nil 段 = 本次未提交（保存时保留现状），不参与校验
	if cfg.Base != nil && (cfg.Base.Port < 0 || cfg.Base.Port > 65535) {
		return errors.New("base.port out of range (0-65535)")
	}
	if cfg.Storage != nil && cfg.Storage.MaxSize < 0 {
		return errors.New("storage.max_size must be >= 0")
	}
	if cfg.Transfer != nil && cfg.Transfer.MaxCount < 0 {
		return errors.New("transfer.max_count must be >= 0")
	}
	if cfg.Transfer != nil && cfg.Transfer.ExpireDefault < 0 {
		return errors.New("transfer.expire_default must be >= 0")
	}
	if err := validateUserSettings(cfg.User); cfg.User != nil && err != nil {
		return err
	}
	// 出站目标保存期校验（2026-10-05 审计 P3：notify/oidc 段此前零校验，
	// 管理员级 SSRF 至少收紧 scheme/私网语义——与存储端点同一策略开关）
	if cfg.Notify != nil {
		if u := strings.TrimSpace(cfg.Notify.WebhookURL); u != "" {
			if err := security.ValidateEndpointURL(u); err != nil {
				return fmt.Errorf("notify.webhook_url 校验失败: %w", err)
			}
		}
		if h := strings.TrimSpace(cfg.Notify.SMTP.Host); h != "" {
			if err := security.ValidateEndpointHost(h); err != nil {
				return fmt.Errorf("notify.smtp.host 校验失败: %w", err)
			}
		}
	}
	if cfg.OIDC != nil {
		if u := strings.TrimSpace(cfg.OIDC.Issuer); u != "" {
			if err := security.ValidateEndpointURL(u); err != nil {
				return fmt.Errorf("security.oidc.issuer 校验失败: %w", err)
			}
		}
	}
	// local_import roots 白名单校验（2026-10-05 审计 P3：roots=["/"] 时任意
	// 登录用户可经 import-local 导入服务器任意可读文件）
	for _, li := range []*conf.LocalImportConfig{cfg.LocalImport, localImportOfUploadEx(cfg.UploadEx)} {
		if li == nil {
			continue
		}
		for _, root := range li.Roots {
			root = strings.TrimSpace(root)
			if root == "" {
				continue
			}
			if !filepath.IsAbs(root) {
				return fmt.Errorf("local_import.roots 需为绝对路径: %s", root)
			}
			if filepath.Clean(root) == "/" {
				return errors.New("local_import.roots 禁止配置根目录 /")
			}
		}
	}
	// p2p.federation 段校验（2026-10-14 对称节点 P1）：URL 收紧 scheme/形态
	// （与 notify/oidc 同一策略），registry 支持逗号多主备
	if cfg.P2P != nil && cfg.P2P.Federation != nil {
		f := cfg.P2P.Federation
		if u := strings.TrimSpace(f.RegistryURL); u != "" {
			for _, one := range strings.Split(u, ",") {
				one = strings.TrimSpace(one)
				if one == "" {
					continue
				}
				if err := security.ValidateEndpointURL(one); err != nil {
					return fmt.Errorf("p2p.federation.registry_url 校验失败: %w", err)
				}
			}
		}
		if u := strings.TrimSpace(f.PublicURL); u != "" {
			if err := security.ValidateEndpointURL(u); err != nil {
				return fmt.Errorf("p2p.federation.public_url 校验失败: %w", err)
			}
		}
		if f.AnnounceMinEntropyBits < 0 || f.AnnounceMinEntropyBits > 256 {
			return errors.New("p2p.federation.announce_min_entropy_bits out of range (0-256)")
		}
	}
	// p2p.direct 子段校验（P2a）：roots 绝对路径+禁 "/"（与 local_import 同策略）
	if cfg.P2P != nil && cfg.P2P.Direct != nil {
		d := cfg.P2P.Direct
		for _, root := range d.SendRoots {
			root = strings.TrimSpace(root)
			if root == "" {
				continue
			}
			if !filepath.IsAbs(root) {
				return fmt.Errorf("p2p.direct.send_roots 需为绝对路径: %s", root)
			}
			if filepath.Clean(root) == "/" {
				return errors.New("p2p.direct.send_roots 禁止配置根目录 /")
			}
		}
		if d.RecvDir != "" && !filepath.IsAbs(d.RecvDir) {
			return fmt.Errorf("p2p.direct.recv_dir 需为绝对路径: %s", d.RecvDir)
		}
		if d.MaxTaskBytes < 0 || d.InboxQuotaBytes < 0 {
			return errors.New("p2p.direct 配额参数必须 >= 0")
		}
		if u := strings.TrimSpace(d.RegistryURL); u != "" {
			if err := security.ValidateEndpointURL(u); err != nil {
				return fmt.Errorf("p2p.direct.registry_url 校验失败: %w", err)
			}
		}
	}
	return nil
}

// P2PSection P2P 能力配置段（spec 2026-10-14 §5）。
type P2PSection struct {
	// Federation 联邦控制面（overlay → conf.Federation；nil = 保留现状）
	Federation *conf.FederationConfig `json:"federation,omitempty"`
	// Direct 设备直传数据面（p2pc；nil = 保留现状；P2a 落地；类型归 conf 层——
	// p2px 域消费须遵守跨域依赖白名单，不入 app 域）
	Direct *conf.P2PDirectConfig `json:"direct,omitempty"`
}

// localImportOfUploadEx UploadEx 段内嵌的 local_import（同一配置两种提交形状）
func localImportOfUploadEx(u *conf.UploadConfig) *conf.LocalImportConfig {
	if u == nil {
		return nil
	}
	return &u.LocalImport
}

// validateUserSettings 用户设置合法性（负数一律拒绝；0 语义为"不限/默认"）。
func validateUserSettings(u *UserSettings) error {
	if u == nil {
		return errors.New("user settings is nil")
	}
	if u.UserUploadSize < 0 || u.UserStorageQuota < 0 {
		return errors.New("user upload size / storage quota must be >= 0")
	}
	if u.SessionExpiryHours < 0 || u.SessionExpiryHours > 24*365 {
		return errors.New("session expiry hours out of range (0-8760)")
	}
	return nil
}
