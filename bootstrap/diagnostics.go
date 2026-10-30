package bootstrap

// 部署自检快照（2026-10-30，第一趟 4.2.0）—— 供官方 CLI `pb doctor` 消费。
//
// # 为什么要有这个端点
//
// 9 种部署面（docker / charts / fnos / openwrt / qnap / synology / ugreen /
// terramaster / desktop）差异极大：端口、存储后端、Redis 有无、随包 Beam 与否
// 都不一样。用户「装完不ら」时，缺的不是日志，而是**一条能给出结论的命令**。
// 而这些事实（DB 是否可达、当前生效的存储后端、Redis 模式、部署模式、版本）
// 此前只能从日志里挖——日志是给人读的，不是给工具读的。
//
// ⇒ 本端点把「服务端的运行事实」结构化出来，诊断逻辑**集中在 core 一处**
// （不在 server 侧散落多套权限判断与探测代码）。
//
// # 暴露面
//
// 管理面端点，鉴权 = `middleware.AdminOrAdminKey()`（admin JWT **或 admin 用户的
// API Key**）——与 /admin/p2p/* 同款口径：官方 CLI 用的就是 admin API Key
// （server/cmd/pb 的 p2p 子命令已是这个口径）。
//
// 双重兜底：① 注册条件 `config.ServesAdminPlane()`（public 副本不注册）；
// ② public 副本的 adminPlaneGuard 按路径前缀拦 /admin/*（即使漏注册也 404）。
//
// **只读**：不写任何状态、不改配置、不触发迁移。
//
// # 与能力快照的关系
//
// 能力（capabilities.go）= 「支持什么」；本报告 = 「现在是什么状态」。
// 两者共用同一批生效值来源（config / 存储单例 / setup 状态）⇒ 不会各说各话。

import (
	"context"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/oekit/core/version"

	"github.com/pigeonbox/core/pkg/resp"
)

// DiagnosticsReport 部署自检报告（一份快照）。
type DiagnosticsReport struct {
	// Version **构建版本**（oekit/core/version.Version，由 ldflags 注入）。
	// ⚠️ 不是 config.App.Version —— 那是 yaml 里的 app.version（缺省 1.0.0，
	// 常年不更新）⇒ 真机上 doctor 会把 4.2.0 的实例报成「1.0.0」。
	// 版本披露用于 CVE 匹配，必须与 /version 端点**同源**。
	Version string `json:"version"`
	// BuildCommit 构建 commit（版本对齐用）。
	BuildCommit string `json:"build_commit,omitempty"`
	// BuildTime 构建时间。
	BuildTime string `json:"build_time,omitempty"`
	// DeploymentMode 部署模式：standalone | public | admin。
	DeploymentMode string `json:"deployment_mode"`
	// ServerTime 服务端当前时间（CLI 用它与本机时钟比对 ⇒ 时钟偏差）。
	ServerTime time.Time `json:"server_time"`
	// Database 数据库连通性。
	Database DiagnosticsDatabase `json:"database"`
	// Storage 存储后端状态。
	Storage DiagnosticsStorage `json:"storage"`
	// Redis 缓存模式。
	Redis DiagnosticsRedis `json:"redis"`
	// Site 站点对外地址状态。
	Site DiagnosticsSite `json:"site"`
	// Setup 首启状态。
	Setup DiagnosticsSetup `json:"setup"`
	// Capabilities 能力快照（生效值）。
	Capabilities CapabilitySnapshot `json:"capabilities"`
}

// DiagnosticsDatabase 数据库连通性。
type DiagnosticsDatabase struct {
	// Driver 驱动名（sqlite / mysql / postgres）。
	Driver string `json:"driver"`
	// Reachable 是否可连通（Ping 成功）。
	Reachable bool `json:"reachable"`
	// Error 连通失败原因（reachable=true 时为空）。
	Error string `json:"error,omitempty"`
}

// DiagnosticsStorage 存储后端状态。
type DiagnosticsStorage struct {
	// Backend **生效**后端（远端构造失败会降级 local ⇒ 这里与 Configured 可能不同）。
	Backend string `json:"backend"`
	// Configured 配置里指定的后端。
	Configured string `json:"configured"`
	// Degraded 是否降级（Backend ≠ Configured ⇒ 远端构造失败回落 local）。
	Degraded bool `json:"degraded"`
	// DataPath 本地存储根目录（远端模式下仍承载分片合并副本）。
	DataPath string `json:"data_path,omitempty"`
	// InitError 降级原因（无降级时为空）。
	InitError string `json:"init_error,omitempty"`
}

// DiagnosticsRedis 缓存模式。
type DiagnosticsRedis struct {
	// Mode redis | memory。
	Mode string `json:"mode"`
	// Address Redis 地址（mode=memory 时为空）。
	Address string `json:"address,omitempty"`
}

// DiagnosticsSite 站点对外地址状态。
type DiagnosticsSite struct {
	// PublicURL 生效的站点公网地址（未配置时为空）。
	PublicURL string `json:"public_url,omitempty"`
	// PublicURLSet 是否已配置。
	PublicURLSet bool `json:"public_url_set"`
}

// DiagnosticsSetup 首启状态。
type DiagnosticsSetup struct {
	// Initialized 是否已初始化（已有 admin）。
	Initialized bool `json:"initialized"`
	// TokenRequired 建号是否需要令牌。
	TokenRequired bool `json:"token_required"`
	// TokenSource 令牌来源：env | auto | none。
	TokenSource string `json:"token_source"`
}

// setupTokenSource 令牌来源（env=显式 env；auto=启动时自动生成的一次性令牌；none=未启用）。
func setupTokenSource() string {
	if setupToken() != "" {
		return "env"
	}
	if p := setupAutoToken.Load(); p != nil && *p != "" {
		return "auto"
	}
	return "none"
}

// diagnosticsReport 采集部署自检报告（纯函数式，便于单测）。
func diagnosticsReport(ctx context.Context) *DiagnosticsReport {
	report := &DiagnosticsReport{
		DeploymentMode: "standalone",
		ServerTime:     time.Now(),
	}
	report.Version = version.Version
	report.BuildCommit = version.BuildCommit
	report.BuildTime = version.BuildTime
	if config != nil {
		report.DeploymentMode = config.NormalizedDeploymentMode()
	}

	// ===== 数据库 =====
	switch database {
	case nil:
		report.Database = DiagnosticsDatabase{Error: "database not initialized"}
	default:
		report.Database.Driver = database.Name()
		sqlDB, err := database.DB()
		if err != nil {
			report.Database.Error = err.Error()
			break
		}
		if err := sqlDB.Ping(); err != nil {
			report.Database.Error = err.Error()
			break
		}
		report.Database.Reachable = true
	}

	// ===== 存储 =====
	if config != nil {
		report.Storage.Configured = string(config.Storage.Type)
	}
	if svc := getBootstrapStorageService(); svc != nil {
		report.Storage.Backend = string(svc.EffectiveType())
		report.Storage.DataPath = svc.DataPath()
		if err := svc.InitError(); err != nil {
			report.Storage.InitError = err.Error()
		}
		report.Storage.Degraded = report.Storage.Backend != report.Storage.Configured
	}

	// ===== Redis =====
	report.Redis.Mode = redisModeOf()
	if report.Redis.Mode == "redis" && config != nil {
		report.Redis.Address = config.Redis.Addr()
	}

	// ===== 站点地址 =====
	report.Site.PublicURL = effectivePublicURL(ctx)
	report.Site.PublicURLSet = report.Site.PublicURL != ""

	// ===== 首启 =====
	if publicSetupSvc != nil {
		if initialized, err := publicSetupSvc.IsSystemInitialized(ctx); err == nil {
			report.Setup.Initialized = initialized
		}
	}
	report.Setup.TokenRequired = effectiveSetupToken() != ""
	report.Setup.TokenSource = setupTokenSource()

	report.Capabilities = capabilitySnapshot(ctx)
	return report
}

// diagnosticsHandler 部署自检端点（GET /admin/diagnostics，管理面）。
//
// 鉴权在注册处（middleware.AdminOrAdminKey）——本函数只负责采集与应答。
func diagnosticsHandler(ctx context.Context, c *app.RequestContext) {
	resp.Success(c, diagnosticsReport(ctx))
}
