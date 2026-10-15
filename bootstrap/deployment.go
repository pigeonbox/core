package bootstrap

// 多副本部署模式支持（deployment.mode = public | admin；standalone 走历史路径）。
// 设计与通信协议见 docs/specs/2026-10-06-multi-replica-deployment-modes.md：
//   - public 副本只注册公开面路由，管理面路径由门卫中间件 404；
//   - admin 实例独占后台任务/DB 迁移/system_configs 写权限，变更经 Redis 广播；
//   - public 副本订阅广播后失效配置缓存并对存储单例热 Reload。
// 单机 standalone 不经过本文件的任何运行期逻辑（仅约束校验早退）。

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/pigeonbox/contracts/errcode"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/gen/router"
	genadmin "github.com/pigeonbox/core/gen/router/admin"
	genchunk "github.com/pigeonbox/core/gen/router/chunk"
	gencommon "github.com/pigeonbox/core/gen/router/common"
	genhealth "github.com/pigeonbox/core/gen/router/health"
	genmaintenance "github.com/pigeonbox/core/gen/router/maintenance"
	gennotify "github.com/pigeonbox/core/gen/router/notify"
	genpresign "github.com/pigeonbox/core/gen/router/presign"
	genpreview "github.com/pigeonbox/core/gen/router/preview"
	genqrcode "github.com/pigeonbox/core/gen/router/qrcode"
	genratelimit "github.com/pigeonbox/core/gen/router/ratelimit"
	genrequest "github.com/pigeonbox/core/gen/router/request"
	gensetup "github.com/pigeonbox/core/gen/router/setup"
	genshare "github.com/pigeonbox/core/gen/router/share"
	genanon "github.com/pigeonbox/core/gen/router/share_anonymous"
	genstorage "github.com/pigeonbox/core/gen/router/storage"
	genuser "github.com/pigeonbox/core/gen/router/user"
	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/repo/db"
	"github.com/pigeonbox/core/repo/redis"
	"github.com/pigeonbox/core/storage"
	"github.com/pigeonbox/kit/async"
	"go.uber.org/zap"

	configApp "github.com/pigeonbox/core/app/config"
	oidcApp "github.com/pigeonbox/core/app/oidc"
	customHandler "github.com/pigeonbox/core/transport/http/handler"
)

// applyDeploymentConstraints 部署模式校验与派生约束。
// 非法模式 fail-fast；public/admin 模式下的硬约束：
//   - SQLite 单写者不支持多进程共享，强制 mysql/postgresql；
//   - federation 节点身份是进程级 Ed25519 密钥，多副本语义未定义（拍板：暂不支持），自动降级并告警；
//   - public 副本强制关闭 DB 迁移（AutoMigrate/版本化迁移只在 admin/standalone 执行，防多副本竞态）。
func applyDeploymentConstraints(cfg *conf.AppConfiguration) error {
	mode := cfg.NormalizedDeploymentMode()
	switch mode {
	case conf.DeployModeStandalone:
		return nil
	case conf.DeployModePublic, conf.DeployModeAdmin:
	default:
		return fmt.Errorf("invalid deployment.mode %q: must be standalone|public|admin", cfg.Deployment.Mode)
	}

	logger.Info("deployment mode enabled", zap.String("mode", mode))
	if cfg.Database.Driver == "sqlite" {
		return fmt.Errorf("deployment.mode=%s requires database.driver=mysql|postgres (SQLite 为单写者，不支持多进程共享)", mode)
	}
	if cfg.Federation.Enabled {
		cfg.Federation.Enabled = false
		logger.Warn("federation disabled: node identity is process-level (Ed25519), multi-replica semantics undefined")
	}
	if mode == conf.DeployModePublic {
		cfg.Database.AutoMigrate = false
		cfg.Database.Migrate = false
	}
	if cfg.Redis.Host == "" {
		// 多副本硬约束运行时化（values.yaml 已声明，此前仅 Warn）：无 Redis 时
		// 配置广播失效 + 取件码映射/presign 会话/限流计数按副本分裂，静默运行
		// 的后果比启动失败严重
		return fmt.Errorf("deployment.mode=%s requires redis.host (配置广播与跨实例状态共享依赖 Redis，缺失会导致多副本状态分裂)", mode)
	}
	return nil
}

// deploymentGate 部署模式门卫（public 副本专用）：管理面路径一律 404。
//   - 未按模式注册的组（admin/storage/...）由此获得干净 404（而非 SPA 回退）；
//   - notify/health 等混合生成组中已注册的管理路由（/admin/notifies、/version）
//     被物理拦截——混合组不能按面拆开注册（生成物），门卫是兜底防线。
//
// 清单由 TestDeploymentModes 守卫：实际注册的任何管理面路径必须命中本判定，防漂移。
func deploymentGate() app.HandlerFunc {
	if config == nil || !config.IsPublicReplica() {
		return nil
	}
	return func(ctx context.Context, c *app.RequestContext) {
		if isAdminPlanePath(string(c.Request.URI().Path())) {
			c.JSON(consts.StatusNotFound, map[string]interface{}{"code": errcode.CodeNotFound, "message": "not found"})
			c.Abort()
			return
		}
		c.Next(ctx)
	}
}

// isAdminPlanePath 管理面路径判定（public 副本拒绝面）。
// 注意 /admin 同时是前端 SPA 的页面路径前缀，但 API 请求（JSON）与页面导航
// 语义不同：public 副本上管理 API 不可达，浏览器直接访问 /admin/** 页面路径
// 由前端路由接管（NoRoute 回退发生在门卫放行之后……此处不回退）。
// 权衡：管理页面上 public 副本也不可用了——多副本拓扑中管理控制台一律走
// admin 实例入口（独立 Ingress/port-forward），这是拍板行为。
func isAdminPlanePath(path string) bool {
	return strings.HasPrefix(path, "/admin") ||
		strings.HasPrefix(path, "/setup") ||
		path == "/version" ||
		path == "/api/v1/mcp"
}

// registerGeneratedRoutes 按部署模式注册 IDL 生成路由组。
// gen/router/register.go 的 GeneratedRegister 是全量薄组合（生成物，勿改），
// 这里直接挑选组函数等价注册。⚠️ scripts/gen-router.sh 再生成新增组时须同步
// 本函数分组清单（守卫测试 TestRegisteredRoutesMatchContract 保证 standalone
// 契约不缺；public/admin 清单由 TestDeploymentModes 断言关键路由）。
func registerGeneratedRoutes(h *server.Hertz) {
	switch config.NormalizedDeploymentMode() {
	case conf.DeployModePublic:
		genanon.Register(h)
		genpresign.Register(h)
		gennotify.Register(h)
		gencommon.Register(h)
		genhealth.Register(h)
		genqrcode.Register(h)
		genchunk.Register(h)
		genuser.Register(h)
		genshare.Register(h)
		genpreview.Register(h)
		// 寄件码域（2026-10-09 IDL 化）：链接管理（JWT）+ 访客公开投递，
		// 原 customHandler 注册在 public 面条件内，语义保持
		genrequest.Register(h)
	case conf.DeployModeAdmin:
		genratelimit.Register(h)
		gensetup.Register(h)
		genstorage.Register(h)
		genadmin.Register(h)
		genmaintenance.Register(h)
		// notify 是混合生成组：管理 CRUD（/admin/notifies）在管理面注册，
		// 公开 /notifies/active 随组进入 admin 实例（无害，门卫只拦 public 侧）
		gennotify.Register(h)
		genhealth.Register(h)
		gencommon.Register(h)
	default:
		router.GeneratedRegister(h)
	}
}

// stripAdminPlanePaths public 模式下从合并后的 OpenAPI spec 剥离管理面路径：
// IDL schema 是全量契约，公开端点清单不应枚举本副本不可达的管理端点。
func stripAdminPlanePaths(spec []byte) []byte {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(spec, &doc); err != nil {
		return spec
	}
	var paths map[string]json.RawMessage
	raw, ok := doc["paths"]
	if !ok || json.Unmarshal(raw, &paths) != nil {
		return spec
	}
	stripped := false
	for p := range paths {
		if isAdminPlanePath(p) {
			delete(paths, p)
			stripped = true
		}
	}
	if !stripped {
		return spec
	}
	b, err := json.Marshal(paths)
	if err != nil {
		return spec
	}
	doc["paths"] = b
	out, err := json.Marshal(doc)
	if err != nil {
		return spec
	}
	return out
}

// ===== 配置变更广播（Redis pubsub + revision 对账）=====

const (
	// configChangeChannel 管理端配置/存储变更通知频道（广播语义，副本各自订阅）。
	configChangeChannel = "pb:config:changed"
	// configRevisionKey 变更版本号（INCR）。pubsub 是 fire-and-forget，副本以
	// 30s 周期对账 revision 兜底订阅断线窗口内丢消息。
	configRevisionKey = "pb:config:revision"
)

// propagatedRevision 已应用的最新变更版本（-1/0 = 未初始化或无历史变更）。
var propagatedRevision atomic.Int64

// publishConfigChanged admin 实例侧：配置持久化后发布变更通知
// （经 configApp.Service.SetOnConfigPersisted 注入）。
func publishConfigChanged() {
	rdb := redis.GetClient()
	if rdb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rdb.Incr(ctx, configRevisionKey)
	rdb.Publish(ctx, configChangeChannel, "changed")
}

// startConfigPropagationSubscriber public 副本侧：订阅广播 + 30s revision 对账。
// 订阅断线自动重连；启动时取当前 revision 为基线，不回放启动前的历史变更
// （启动加载链路已含最新持久化配置）。
func startConfigPropagationSubscriber() {
	rdb := redis.GetClient()
	if rdb == nil {
		logger.Warn("config propagation disabled: Redis unavailable")
		return
	}
	if rev, err := rdb.Get(context.Background(), configRevisionKey).Int64(); err == nil {
		propagatedRevision.Store(rev)
	} else {
		propagatedRevision.Store(0)
	}
	// 常驻订阅/对账循环无监督者，applyPropagatedConfig 涉及存储 Reload/通知
	// 渠道/OIDC 重建，panic 面非空——GoSafe 兜底避免带崩整个副本进程。
	async.GoSafe(func() {
		for {
			sub := rdb.Subscribe(context.Background(), configChangeChannel)
			for range sub.Channel() {
				maybeApplyConfig("pubsub")
			}
			_ = sub.Close()
			logger.Warn("config propagation subscription lost, retrying in 5s")
			time.Sleep(5 * time.Second)
		}
	})
	async.GoSafe(func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			maybeApplyConfig("poll")
		}
	})
	logger.Info("config propagation subscriber started", zap.String("channel", configChangeChannel))
}

// maybeApplyConfig revision 前进时应用一次广播配置（幂等；并发触达由
// revision 单调性兜底，最坏情况重复应用一次等效 Reload）。
func maybeApplyConfig(source string) {
	rdb := redis.GetClient()
	if rdb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	rev, err := rdb.Get(ctx, configRevisionKey).Int64()
	cancel()
	if err != nil {
		return // 键不存在（从未变更）或 Redis 抖动：静默，下轮再对账
	}
	prev := propagatedRevision.Load()
	if prev >= 0 && rev <= prev {
		return
	}
	applyPropagatedConfig()
	propagatedRevision.Store(rev)
	logger.Info("config change applied from broadcast",
		zap.String("source", source), zap.Int64("revision", rev))
}

// applyPropagatedConfig 应用管理端广播的配置变更：重读 system_configs →
// overlay 全局 conf → 存储单例 Reload → 通知渠道/OIDC service 重建。
// 与管理端本进程保存路径（UpdateConfig + applySystemConfigOverlayLocked +
// ReconfigureHooks）逐项同构；域服务持有同一存储单例指针，Reload 原地生效。
func applyPropagatedConfig() {
	cfgSvc := configApp.Default()
	cfgSvc.InvalidateRuntimeConfig()
	cfgSvc.RestoreAdminSettings() // 重读 DB → 内存缓存 + 全局 conf overlay + 会话时长
	restoreRuntimeStorage()       // DB 存储段 → 全局 conf（env 覆盖优先级保持）
	if svc := getBootstrapStorageService(); svc != nil {
		if err := svc.Reload(storage.ConfigFromConf(&config.Storage, bootstrapBaseURL())); err != nil {
			logger.Error("propagated storage reload failed", zap.Error(err))
		}
	}
	if notifySvcInstance != nil {
		applyNotifyConfig(notifySvcInstance, &config.Notify)
	}
	o := &config.Security.OIDC
	customHandler.SetOIDCService(oidcApp.NewService(db.GetDB(), oidcApp.Config{ // 装配层显式句柄
		Enabled:          o.Enabled,
		Issuer:           o.Issuer,
		ClientID:         o.ClientID,
		ClientSecret:     o.ClientSecret,
		Scopes:           o.Scopes,
		FrontendCallback: o.FrontendCallback,
	}))
}
