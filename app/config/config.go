// config.go 站点配置模型与校验:SystemConfig/UserSettings 类型、默认值、
// 写库前校验(纯函数簇,自 admin 域原样迁入)。
package config

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/security"
	"github.com/pigeonbox/core/pkg/utils"
)

// BaseConfig 站点基础信息段。指针语义与其余段一致：nil = 本次未提交，
// UpdateConfig 保留现状（值类型无法表达"未提交"，局部保存曾把整段清零）。
type BaseConfig struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Port        int    `json:"port"`

	// PublicURL 站点公网地址（2026-10-22 落地，可选）：用户可访问本柜的对外
	// 地址（如 https://box.example.com）。分享链接与「交付文本」按它拼；
	// 为空 = 未配置 = 前端回退浏览器当前 origin。
	//
	// 为什么需要：分享链接原先按**请求 origin** 拼（frontend-core
	// ShareResultDialog.vue），NAS/内网部署时管理员从内网访问 → 生成的链接
	// 带内网 IP（如 http://10.10.10.3:56789）→ 发给外部用户根本打不开。
	// 这是"多部署方式"下最隐蔽的易用性断点。
	PublicURL string `json:"public_url"`
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
	cfg.Transfer = &TransferSettings{MaxCount: 100, ExpireDefault: 7}          // 7天
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
	// base.public_url 站点公网地址（2026-10-22）：可选，空 = 未配置。
	if cfg.Base != nil {
		if u := strings.TrimSpace(cfg.Base.PublicURL); u != "" {
			if err := validatePublicSiteURL(u); err != nil {
				return fmt.Errorf("base.public_url 校验失败: %w", err)
			}
		}
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
	// upload.allowed_expire_styles 校验（2026-10-22）：合法样式集合以
	// pkg/utils.AllExpireStyles 为准（唯一真相源）。
	//
	// 为什么要校验：管理台此前**没有**这个字段的编辑器（后端 API 接受、界面不暴露）
	// ⇒ 管理员改不动；更糟的是写入非法样式后，bootstrap 的
	// effectiveExpireStyles 会把它**静默过滤掉** ⇒ 管理员看到"保存成功"但设置
	// 没有任何效果，且无任何提示。现在保存即报错并列出合法值。
	if cfg.UploadEx != nil {
		for _, st := range cfg.UploadEx.AllowedExpireStyles {
			if !utils.IsValidExpireStyle(st) {
				return fmt.Errorf("upload.allowed_expire_styles 含非法样式 %q（合法值: %s）",
					st, strings.Join(utils.AllExpireStyles(), "/"))
			}
		}
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
		// notify.bark.server_url 保存期校验（2026-11-01 Telegram/Bark 渠道；
		// telegram 目标 host 固定 api.telegram.org 无出站 URL 注入面，无需校验）
		if u := strings.TrimSpace(cfg.Notify.Bark.ServerURL); u != "" {
			if err := security.ValidateEndpointURL(u); err != nil {
				return fmt.Errorf("notify.bark.server_url 校验失败: %w", err)
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
		// 可投递对端（联邦前台化，2026-11-01）：URL 形态 + 口令必填 +
		// 私网**按对端粒度**放行（allow_private=true 的对端跳过私网判定，
		// 但 scheme/host 照样校验）。
		//
		// ⚠️ 不用全局 security.ssrf.allow_private_networks 兜底：全局一开 =
		// 所有出站点位都放行私网（面太大）；这里只放行管理员**明确配的那几个**。
		for i, peer := range f.DeliveryPeers {
			u := strings.TrimSpace(peer.URL)
			if u == "" {
				return fmt.Errorf("p2p.federation.delivery_peers[%d].url 不能为空", i)
			}
			if strings.TrimSpace(peer.Token) == "" {
				// 口令即权限：没有口令的对端既无法被授权，也无法被对方柜认。
				return fmt.Errorf("p2p.federation.delivery_peers[%d].token 不能为空（口令即权限）", i)
			}
			if peer.AllowPrivate {
				if err := security.ValidateEndpointURLAllowPrivate(u); err != nil {
					return fmt.Errorf("p2p.federation.delivery_peers[%d].url 校验失败: %w", i, err)
				}
				continue
			}
			if err := security.ValidateEndpointURL(u); err != nil {
				return fmt.Errorf("p2p.federation.delivery_peers[%d].url 校验失败: %w", i, err)
			}
		}
	}
	return nil
}

// P2PSection P2P 能力配置段（spec 2026-10-14 §5）。
//
// 2026-10-22 Wave B'：直传段（direct）随 Box 侧直传编排层整体退役 —— 本段
// 只剩联邦段。存量 DB 里若仍有 p2p.direct.*（尤其 registry_url），该段被
// **忽略**并在启动日志响亮提示（见 stale.go）；注册中心请改配
// p2p.federation.registry_url。
type P2PSection struct {
	// Federation 联邦控制面（overlay → conf.Federation；nil = 保留现状）
	Federation *conf.FederationConfig `json:"federation,omitempty"`
}

// localImportOfUploadEx UploadEx 段内嵌的 local_import（同一配置两种提交形状）
func localImportOfUploadEx(u *conf.UploadConfig) *conf.LocalImportConfig {
	if u == nil {
		return nil
	}
	return &u.LocalImport
}

// validatePublicSiteURL 校验「站点公网地址」形态。
//
// 与 security.ValidateEndpointURL **刻意不同**：后者带 SSRF 私网拦截
// （那个 URL 会被服务端主动取回），而站点公网地址只被**展示与复制**，
// 服务端从不取它——局域网内的 NAS 填 http://192.168.1.10:12345 是正常用法，
// 不能被私网策略拦下。故这里只收紧 scheme 与 host 形态，挡掉
// javascript:/data: 这类可执行 scheme 与空 host。
func validatePublicSiteURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("URL 解析失败: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("仅支持 http/https 协议，当前为 %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return errors.New("缺少 host")
	}
	return nil
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
