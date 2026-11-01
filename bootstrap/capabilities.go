package bootstrap

// 能力自描述（2026-10-30，第一趟 4.2.0）——「这个部署面支持什么」的一份真相源。
//
// # 为什么要有
//
// 「本机支持哪些能力」此前由**各部署面各写一份**条件判断拼出来：
//
//   - 多副本（public×N + admin×1）下直传/联邦**架构上不可用**
//     （bootstrap/deployment.go 的降级分支：多副本时联邦被强制关闭）；
//   - charts 的 p2p.enabled / relay.enabled **缺省关**；
//   - NAS 三面随包 PigeonBeam（/beam 已挂载），docker / charts / 桌面端没有。
//
// ⇒ 2026-10-22 设计稿 Q1 拍板的「**没有就是没有、不给坏入口**」无法统一执行；
// 新增一个部署面 = 再抄一份条件判断（而"抄漏了"不会有任何编译期信号）。
//
// # 事实分两侧（别把服务端不知道的事塞进来）
//
// 能力**不是**「服务端全知道」：
//
//   - **服务端事实**（本文件）：部署模式、联邦是否启用、站点公网地址是否配置、
//     首启是否需要令牌、存储后端、Redis 模式。
//   - **客户端事实**（前端探测，**不进本快照**）：
//     ① WebCrypto 是否可用 —— 端到端加密的前提是**安全上下文**，服务端无从得知
//     （frontend-core/src/utils/e2e.ts 的 e2eSupported(scope) 已在做这件事）；
//     ② Beam 入口是否挂载 —— /beam 是**平台适配器**挂在 Box 服务根上的
//     （fnos/qnap/openwrt 三仓同构），Box 服务端自己不知道有没有
//     （frontend-core/src/components/share/deliveryPath.ts 的探测已在做）。
//
// ⇒ 前端不得因「服务端没说有」就当作「有」，也不得因「服务端没说」就当作「没有」：
// 客户端事实一律走客户端探测。
//
// # 不重复下发
//
// 顶层已有同义字段的（federationEnabled / oidcEnabled / initialized / …）
// **不在本快照里重复**——同一事实两个字段必然漂移。教训是 presignEnabled：
// 它是 presignPolicy 的派生布尔，两边迟早对不上，2026-10-25 已退役。
//
// # 暴露面分层
//
//   - **公开面**（/api/config）：只放**匿名可见也无害**的四项
//     （deployment_mode / public_url_set / setup_token_required /
//     federation_delivery）；
//   - **管理面**（/admin/diagnostics）：完整快照（含存储/Redis/DB/版本），
//     见 bootstrap/diagnostics.go。

import (
	"context"
	"strings"

	configApp "github.com/pigeonbox/core/app/config"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/storage"
)

// CapabilitySnapshot 描述「本实例实际支持什么」。
//
// ⚠️ 字段语义一律是**生效值**（runtime 真实状态），不是配置回显——
// 后者会与实际漂移（如后端启动后才起 Redis、远端存储构造失败降级 local）。
type CapabilitySnapshot struct {
	// DeploymentMode 部署模式：standalone | public | admin。
	// 多副本拓扑下前端据此知道「本副本是公开面/管理面」，不再各面各猜。
	DeploymentMode string `json:"deployment_mode"`
	// PublicURLSet 站点公网地址是否已配置（base.public_url 或 server.base_url）。
	// 未配置时分享链接按**请求来源**拼 ⇒ 内网部署会把内网 IP 发给外部用户。
	PublicURLSet bool `json:"public_url_set"`
	// SetupTokenRequired 首启建号是否需要令牌（自动生成的一次性令牌或 PB_SETUP_TOKEN）。
	// true = 未初始化实例的 /setup 会被门禁拦下（前端据此提示「令牌在启动日志里」）。
	SetupTokenRequired bool `json:"setup_token_required"`
	// Federation 联邦是否启用（**生效值**：多副本下自动降级关闭）。
	Federation bool `json:"federation"`
	// FederationDelivery 跨站投递是否可用（联邦前台化，2026-11-01）。
	//
	// 语义 = **联邦启用** 且 **登记了至少一个可投递对端** ⇒ 两者缺一即不可用。
	//
	// ⚠️ 公开面下发这一位**不泄露对端身份**（访客只知道「本站支持跨站投递」，
	// 不知道对端是谁、在哪）⇒ 无害；反之 storage_backend / redis_mode /
	// federation 这几项会泄露部署形态，留在管理面。
	FederationDelivery bool `json:"federation_delivery"`
	// RedisMode redis | memory（redis.host 空 = 单机内存模式）。
	RedisMode string `json:"redis_mode"`
	// StorageBackend 当前存储后端（**生效值**，含远端构造失败后的降级结果）。
	StorageBackend string `json:"storage_backend"`
	// StorageDegraded 是否发生降级（配置的后端 ≠ 生效后端 ⇒ 远端构造失败回落 local）。
	// ⚠️ 这是**真实故障的信号**：用户以为在用 S3，实际全落在本地盘上。
	StorageDegraded bool `json:"storage_degraded"`
}

// redisModeOf 生效的 Redis 模式：redis | memory。
func redisModeOf() string {
	if config != nil && strings.TrimSpace(config.Redis.Host) != "" {
		return "redis"
	}
	return "memory"
}

// effectivePublicURL 生效的站点公网地址（管理台 base.public_url 优先，
// 缺省回退 server.base_url；都没有 = 空串 = 未配置）。
//
// 唯一口径来源：publicConfigHandler（前端 /api/config）与 share/presign 域的
// baseURL provider（bootstrap/wiring.go 的 publicBaseURL）都必须与它一致——
// 否则「用户看到的链接」与「服务端拼的链接」会对不上。
func effectivePublicURL(ctx context.Context) string {
	publicURL := ""
	if config != nil {
		publicURL = strings.TrimSpace(config.Server.BaseURL)
	}
	// DefaultSafe：未 InitDefault 时返回 nil（单元测试 / bootstrap 早期阶段）
	// ⇒ 回退到 server.base_url，而不是 panic。
	if svc := configApp.DefaultSafe(); svc != nil {
		if cfg, err := svc.GetConfig(ctx); err == nil && cfg != nil {
			if u := strings.TrimSpace(cfg.Base.PublicURL); u != "" {
				publicURL = u
			}
		}
	}
	return publicURL
}

// capabilitySnapshot 计算本实例的能力快照（生效值）。
//
// 存储后端取自 bootstrap 的存储单例（与实际读写链路**同一个实例**）——
// 读 conf 会拿到"配置想用哪个"而不是"实际在用哪个"（远端构造失败会降级 local）。
func capabilitySnapshot(ctx context.Context) CapabilitySnapshot {
	snap := CapabilitySnapshot{
		DeploymentMode:     conf.DeployModeStandalone,
		PublicURLSet:       false,
		SetupTokenRequired: false,
		Federation:         false,
		RedisMode:          redisModeOf(),
		StorageBackend:     "local",
	}
	if config != nil {
		snap.DeploymentMode = config.NormalizedDeploymentMode()
	}
	// 联邦段取**生效值**（全局 conf 快照；多副本下 deployment.go 已就地降级
	// 为 false，DB overlay 也会再次强制 ⇒ 两处口径一致）。
	//
	// ⚠️ 不用 bootstrap 的 config 变量：管理台保存配置后 DB overlay 替换的是
	// 全局快照（copy-on-write 换指针），config 变量会**陈旧** ⇒ 会出现
	// 「能力位说不可用、投递面却能用」的自相矛盾。
	fed := effectiveFederationConfig()
	snap.Federation = fed.Enabled
	// 跨站投递能力位（联邦前台化，2026-11-01）：联邦启用 **且** 登记了至少
	// 一个可投递对端 ⇒ 两者缺一即不可用。
	//
	// ⚠️ 对端列表与投递面（app/federation.DeliveryPeers，同一个真相源）同源
	// ⇒ 不会出现「能力位说有、实际没有对端」的错位。
	snap.FederationDelivery = fed.Enabled && len(fed.DeliveryPeers) > 0
	snap.PublicURLSet = effectivePublicURL(ctx) != ""
	snap.SetupTokenRequired = effectiveSetupToken() != ""

	// 存储单例在 config 为 nil 时不可构造（构造要读 config.Storage）⇒ 跳过。
	if config == nil {
		return snap
	}
	// 生效后端取自**真正用于读写**的那个存储单例（bootstrap 单例），而不是配置回显
	// ——远端后端构造失败会降级 local，而配置里仍写着 s3。
	if svc := getBootstrapStorageService(); svc != nil {
		effective := string(svc.EffectiveType())
		snap.StorageBackend = effective
		snap.StorageDegraded = effective != string(storage.StorageType(config.Storage.Type))
	}
	return snap
}

// PublicCapabilities 公开面（/api/config）下发的能力子集。
//
// ⚠️ **独立结构体**，不复用 CapabilitySnapshot：复用会把管理面字段
// （redis_mode / storage_backend / …）以**空串**形态泄到公开 JSON 里
// ⇒ 客户端读到的 `redis_mode: ""` 既不是事实、也不是"未知"的可辨识形态。
type PublicCapabilities struct {
	// DeploymentMode 部署模式：standalone | public | admin。
	DeploymentMode string `json:"deployment_mode"`
	// PublicURLSet 站点公网地址是否已配置。
	PublicURLSet bool `json:"public_url_set"`
	// SetupTokenRequired 首启建号是否需要令牌。
	SetupTokenRequired bool `json:"setup_token_required"`
	// FederationDelivery 跨站投递是否可用（联邦前台化，2026-11-01）。
	//
	// ⚠️ 只暴露「能不能用」，**不暴露对端身份**（谁、在哪）⇒ 访客看得见
	// 功能入口，但看不到管理员配了对端。
	FederationDelivery bool `json:"federation_delivery"`
}

// publicCapabilitySnapshot 公开面（/api/config）下发的能力子集。
//
// 只含**匿名可见也无害**的四项；存储后端/Redis 模式留在管理面诊断端点。
func publicCapabilitySnapshot(ctx context.Context) PublicCapabilities {
	full := capabilitySnapshot(ctx)
	return PublicCapabilities{
		DeploymentMode:     full.DeploymentMode,
		PublicURLSet:       full.PublicURLSet,
		SetupTokenRequired: full.SetupTokenRequired,
		FederationDelivery: full.FederationDelivery,
	}
}

// effectiveFederationConfig 联邦段的**生效值**。
//
// 真相源 = 全局 conf 快照（copy-on-write 原子换指针）：管理台保存配置后，
// DB overlay 更新的是它（app/config/service.go 的 applySystemConfigOverlayLocked
// 整段替换 next.Federation）⇒ bootstrap 的 config 变量是启动时的旧对象，
// 读它会拿到陈旧的对端列表。
//
// 全局快照为 nil（单测 / bootstrap 极早期）时回退 bootstrap 的 config。
func effectiveFederationConfig() conf.FederationConfig {
	if g := conf.Snapshot(); g != nil {
		return g.Federation
	}
	if config != nil {
		return config.Federation
	}
	return conf.FederationConfig{}
}
