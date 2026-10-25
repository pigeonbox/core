package bootstrap

// 职责：应用启动入口与全局状态——Config 别名、staticOpts、GetConfig、
// database/config 全局、Bootstrap/BootstrapWithOptions、Cleanup、ErrInsecureDefaultAdmin。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/oekit/concurrency/async"
	"github.com/oekit/core/lifecycle"
	"github.com/pigeonbox/contracts/openapi"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/auth"
	"github.com/pigeonbox/core/pkg/baseurl"
	"github.com/pigeonbox/core/pkg/gate"
	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/pkg/middleware"
	securityPkg "github.com/pigeonbox/core/pkg/security"
	"github.com/pigeonbox/core/pkg/transfer"
	"github.com/pigeonbox/core/repo/db"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/redis"
	customHandler "github.com/pigeonbox/core/transport/http/handler"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// Config 与 internal/conf 包中的统一配置类型互为别名。
type Config = conf.AppConfiguration

// staticOpts 保存最近一次 Bootstrap 应用的选项(静态服务等闭包读取)。
var staticOpts = defaultOptions()

// GetConfig 获取全局配置
func GetConfig() *Config {
	return config
}

// ErrInsecureDefaultAdmin 生产模式下拒绝以已知默认口令创建默认管理员时返回
// （errors.Is 可判）。逃生门：PB_ADMIN_PASSWORD 注入强密码，或
// PB_DISABLE_DEFAULT_ADMIN=true 走 /setup 首启向导。
var ErrInsecureDefaultAdmin = errors.New("refusing to create default admin with known default password in production mode")

var (
	database *gorm.DB
	config   *Config
)

// Bootstrap 应用程序启动入口(兼容入口,等价于无选项的 BootstrapWithOptions)。
// configPath 为空时依次回退到 CONFIG_PATH 环境变量、默认 configs/config.yaml。
func Bootstrap(configPath string) (*server.Hertz, error) {
	return BootstrapWithOptions(configPath)
}

// BootstrapWithOptions 带函数选项的启动入口。
// 可用选项见 options.go(如 WithStaticDir 覆盖前端静态资源目录)。
func BootstrapWithOptions(configPath string, opts ...Option) (*server.Hertz, error) {
	staticOpts = applyOptions(opts...)

	// 1. 初始化配置
	var err error
	config, err = InitConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to init config: %w", err)
	}

	// 设置全局配置（供其他包访问）
	conf.SetGlobalConfig(config)

	// 1.2 部署模式约束（多副本拆分；非法模式 fail-fast，public 派生约束在此落地）
	if err := applyDeploymentConstraints(config); err != nil {
		return nil, fmt.Errorf("invalid deployment config: %w", err)
	}

	// 1.1 注入 JWT secret 到 auth 包（覆盖硬编码默认值）。
	// 此前 jwt.go 使用硬编码 "PigeonBox2025SecretKey"，且与 config 的 jwt_secret
	// 不一致，导致配置中的 secret 从未生效。此处统一从配置/env 读取。
	auth.SetJWTSecret(config.User.JWTSecret)

	// 2. 初始化日志
	loggerConfig := &logger.Config{
		Level:      config.Log.Level,
		Filename:   config.Log.Filename,
		MaxSize:    config.Log.MaxSize,
		MaxBackups: config.Log.MaxBackups,
		MaxAge:     config.Log.MaxAge,
		Compress:   config.Log.Compress,
	}
	if err := logger.Init(loggerConfig); err != nil {
		return nil, fmt.Errorf("failed to init logger: %w", err)
	}
	logger.Info("JWT secret loaded from configuration",
		zap.Bool("production", config.IsProduction()))

	// 2.1 goroutine panic 兜底接 zap：async.GoSafe 恢复的 panic 带堆栈进结构化日志
	// （默认走 stdlib log；须在最早的业务 goroutine 之前设置，此处为进程内唯一设置点）
	async.SetPanicHandler(func(msg string, stack []byte) {
		logger.Error("goroutine panic recovered",
			zap.String("error", msg), zap.ByteString("stack", stack))
	})

	// 3. 初始化数据库
	database, err = InitDatabase(&config.Database)
	if err != nil {
		return nil, fmt.Errorf("failed to init database: %w", err)
	}

	// 3.05 启动期数据迁移（幂等；必须在对外服务前完成——子表是文件清单的唯一
	// 真相源，存量分享缺子行时取件页会显示空列表）
	RunDataMigrations(database)

	// pkg 层持久化能力注入（pkg/middleware、pkg/transfer 不依赖 repo 层，
	// 由 composition root 以 dao 桥接；须先于任何服务流量）
	middleware.SetAPIKeyStore(daoAPIKeyStore{})
	transfer.SetSink(daoTransferSink{})
	// JWT 身份复核 + 会话纪元（2026-10-05 审计 P2）：封禁/降权/改密即时生效
	middleware.SetIdentityLoader(func(ctx context.Context, userID uint) (*middleware.IdentityRecord, error) {
		u, err := dao.NewUserRepository(database).GetByID(ctx, userID)
		if err != nil {
			return nil, err
		}
		return &middleware.IdentityRecord{Status: u.Status, Role: u.Role, Epoch: u.SessionEpoch}, nil
	})
	auth.SetEpochLoader(func(userID uint) int {
		u, err := dao.NewUserRepository(database).GetByID(context.Background(), userID)
		if err != nil {
			return 0
		}
		return u.SessionEpoch
	})

	// 3.5 初始化 Redis（匿名取件码 / presign 会话 / 分布式限流依赖）。
	// 此前 bootstrap 从不调用 redis.Init，GetClient() 恒为 nil，
	// 导致匿名取件与预签名直传在运行期必然失败（P0）。
	// redis.host 为空 → 单机内存模式：匿名取件/直传会话存进程内 TTL KV
	// （全功能可用，重启丢失、不跨副本共享）。
	// 连不上 → standalone 降级启动并告警（同内存模式语义）；
	// public/admin 多副本硬依赖 Redis（广播/跨实例状态），fail-fast。
	if config.Redis.Host != "" {
		if err := redis.Init(&config.Redis); err != nil {
			if config.NormalizedDeploymentMode() != conf.DeployModeStandalone {
				return nil, fmt.Errorf("deployment.mode=%s requires a reachable redis (addr=%s): %w",
					config.NormalizedDeploymentMode(), config.Redis.Addr(), err)
			}
			logger.Error("Redis init failed — anonymous pickup & presign degraded to in-memory mode",
				zap.String("addr", config.Redis.Addr()), zap.Error(err))
		} else {
			logger.Info("Redis initialized", zap.String("addr", config.Redis.Addr()))
		}
	} else {
		logger.Info("Redis not configured (redis.host empty) — in-memory mode: " +
			"匿名取件/直传会话存进程内（重启丢失，不跨副本共享）")
	}

	// 3.6 安全中间件配置注入：
	//   - 可信代理 CIDR（决定 XFF 是否被采信，防伪造绕过限流）
	//   - 下载令牌签名密钥（复用 jwt_secret 派生，独立注入便于轮换）
	if err := middleware.SetTrustedProxies(config.Security.TrustedProxies); err != nil {
		return nil, fmt.Errorf("invalid security.trusted_proxies: %w", err)
	}
	// 下载令牌签名密钥：优先独立 env（2026-10-05 审计 P3：派生自 jwt_secret
	// 时 JWT 泄露即波及防盗链令牌伪造），缺省回退旧派生保持兼容
	dlSecret := os.Getenv("PB_DOWNLOAD_TOKEN_SECRET")
	if dlSecret == "" {
		dlSecret = "pb-dl:" + config.User.JWTSecret
	}
	securityPkg.SetDownloadTokenSecret(dlSecret)
	// 令牌有效期接配置（2026-10-24：security.download_token.validity_seconds
	// 此前**从未被读取**——pkg/security 里是硬编码 const 1000s，运维改配置无效）。
	securityPkg.SetDownloadTokenTTL(config.Security.DownloadToken.ValiditySeconds)

	// 4. 创建默认管理员（public 副本不执行：管理员初始化归 admin/standalone）
	// PB_DISABLE_DEFAULT_ADMIN=true 时跳过——首启向导（/setup）模式：由访问者
	// 在浏览器完成管理员创建与站点预配置，自建默认口令与向导互斥（2026-10-07）。
	if config.ServesAdminPlane() {
		if os.Getenv("PB_DISABLE_DEFAULT_ADMIN") == "true" {
			logger.Info("default admin creation disabled (PB_DISABLE_DEFAULT_ADMIN=true); use /setup wizard")
		} else if err := CreateDefaultAdmin(database, config); err != nil {
			if errors.Is(err, ErrInsecureDefaultAdmin) {
				// 生产模式拒绝弱口令 admin 是致命错误：带已知口令上线不可接受，
				// 无 admin 静默启动同样不可接受（管理面裸奔）——终止启动。
				return nil, err
			}
			logger.Error("Failed to create default admin", zap.Error(err))
		}
	}

	// 4.5 初始化预览服务
	if err := initPreviewService(); err != nil {
		logger.Error("Failed to init preview service", zap.Error(err))
	}

	// 4.6 初始化新服务（thrift IDL 对应：notify/presign/anonymous/ratelimit）
	initThriftIDLServices(database)

	// 5. 创建 HTTP 服务器
	port := config.Server.Port
	if port == 0 {
		port = 12345
	}
	// 上传 body 上限（应用层强制，覆盖 Hertz 默认无限制）。
	// 配置了 max_file_size（整文件上限，presign 中转同样过 HTTP 层）时取较大值
	// ——修复：local/webdav 下 ≥100MB 走 presign 中转被全局 10MB 拦成 413，
	// 大文件上传链路整条不通（前端 ≥100MB 只走 presign 且无 chunk 回退）。
	uploadSize := int(config.Upload.UploadSize)
	if uploadSize <= 0 {
		uploadSize = 10 * 1024 * 1024 // 默认 10MB
	}
	if maxFile := int(config.Upload.MaxFileSize); maxFile > uploadSize {
		uploadSize = maxFile
	}
	h := server.New(
		server.WithHostPorts(fmt.Sprintf("%s:%d", config.Server.Host, port)),
		server.WithMaxRequestBodySize(uploadSize),
		// 请求体流式透传：presign 中转（upload-direct）按流落盘，大文件不整体进内存
		server.WithStreamBody(true),
	)

	// 可观测性：初始化 Prometheus 指标（在注册中间件前完成）
	if config.Observability.Metrics.Enabled {
		middleware.NewMetrics()
		logger.Info("Prometheus metrics enabled",
			zap.String("path", config.Observability.Metrics.Path))
	}

	// 安全：配置安全响应头。HSTS 三态（2026-10-08 加固）：未配置时生产模式默认
	// 开启、开发默认关——浏览器忽略 HTTP 响应上的 STS 头，纯 HTTP 的 LAN 部署
	// 零影响；自签证书等特殊拓扑显式 enable_hsts=false（或 env PB_ENABLE_HSTS）
	// 即可关闭，恒以显式配置为准。
	hstsEnabled := config.IsProduction()
	if v := config.Security.CORS.EnableHSTS; v != nil {
		hstsEnabled = *v
	}
	middleware.SetSecurityHeadersConfig(middleware.SecurityHeadersConfig{
		EnableHSTS: hstsEnabled,
	})
	if hstsEnabled {
		logger.Info("HSTS enabled (ensure HTTPS deployment)")
	}

	// 全局中间件链（顺序敏感）：
	//   Recovery → RequestID → AccessLog → Metrics → SecurityHeaders → CORS → handler
	//   - Recovery 最外层，捕获任意 panic 转 500，避免进程崩溃
	//   - RequestID 生成/透传 trace_id（X-Trace-Id），写入 ctx 供下游使用
	//   - AccessLog 结构化访问日志（带 trace_id），在 handler 执行后记录 status
	//   - Metrics 采集 RED 指标（延迟/计数/在途），在 handler 执行后记录
	//   - SecurityHeaders 设置 X-Content-Type-Options / X-Frame-Options / HSTS 等
	//   - CORS 跨域，最贴近 handler
	h.Use(middleware.Recovery())
	h.Use(middleware.RequestID())
	h.Use(middleware.AccessLog())
	if config.Observability.Metrics.Enabled {
		h.Use(middleware.MetricsMiddleware())
	}
	h.Use(middleware.SecurityHeaders())
	h.Use(CORS())

	// server.base_url 未配置时按请求来源动态推断公开 base（share/presign 的
	// 分享链接/直传回调地址用）。禁止静态拼接 server.host——那是监听地址
	// （0.0.0.0），对外不可达（2026-10-07 真机事故：分享成功弹窗 0.0.0.0 链接）。
	if config.Server.BaseURL == "" {
		h.Use(publicBaseMiddleware())
	}

	// 部署模式门卫（public 副本拒绝管理面路径；早于限流与全部业务链）
	if gate := deploymentGate(); gate != nil {
		h.Use(gate)
	}

	// 首启门禁（2026-10-22）：建号端点 /setup 的可选令牌 + 按 IP 失败锁定。
	// 默认不要求令牌（保持「打开浏览器走完首启」的零摩擦体验）；设
	// PB_SETUP_TOKEN 后才强制校验。详见 bootstrap/setup_gate.go 的问题陈述。
	h.Use(setupGate())

	// 限流：路径感知，按接口类型选择限流维度（登录/上传/下载）。
	// 防止暴力破解登录、取件码枚举、上传下载 DoS。
	// 路径 → 桶的映射抽为 rateLimitBucket 纯函数（有守卫测试对真实路由表
	// 回归，防止前缀写错导致桶成死条目——2026-10-06 审计发现 /api/v1/chunk
	// 前缀在路由表中不存在，分块上传此前从未进桶）。
	// 配置来自 rate_limit 段（默认值见 middleware.DefaultRateLimitConfig），
	// use_redis=true 且 Redis 可用时多实例共享计数。
	rl := middleware.InitDefaultRateLimiter(middleware.RateLimitConfigFromConf())
	rl.SetRedis(redis.GetClient())
	// 匿名上传日配额计数器复用同一 Redis（无 Redis 退化进程内计数）
	gate.SetQuotaRedis(redis.GetClient())
	h.Use(func(ctx context.Context, c *app.RequestContext) {
		path := string(c.Request.URI().Path())
		switch rateLimitBucket(path) {
		case "login":
			rl.LoginMiddleware()(ctx, c)
		case "upload":
			rl.UploadMiddleware()(ctx, c)
		case "download":
			rl.DownloadMiddleware()(ctx, c)
		default:
			// 未归桶路径全局默认桶兜底（2026-10-08 攻击面加固：此前
			// GlobalMiddleware 从未接线，/api/config、/api/v1/notifies/public、
			// /api/v1/user/refresh 等公开端点零限流可被无成本轰炸）。
			// 探针/静态资源豁免（rateLimitGlobalExempt）。
			if rateLimitGlobalExempt(path) {
				c.Next(ctx)
				return
			}
			rl.GlobalMiddleware()(ctx, c)
		}
	})

	// /version 版本信息不再公开：版本号/commit 是 CVE 匹配的定位器，
	// 前端零消费、探针不走它（健康探针用 /ping /live /ready），收归管理员。
	// 运维侧查版本用 admin 凭证调用（smoke-full.sh 已同步）。
	h.Use(func(ctx context.Context, c *app.RequestContext) {
		if string(c.Request.URI().Path()) == "/version" {
			middleware.AdminMiddleware()(ctx, c)
			return
		}
		c.Next(ctx)
	})

	// 6. 注册路由（按部署模式选组，见 deployment.go；IDL 生成链 scripts/gen-router.sh）
	registerGeneratedRoutes(h)

	// 7. 注册自定义路由
	customizedRegister(h)

	// 8. 生成 openapi.json：骨架规范（运行时路由表，全端点零漂移+认证矩阵）
	// 与 contracts IDL 规范（api.* 注解，带完整 schema）合并——IDL 治理域
	// 以 IDL schema 为准，customizedRegister 手写路由由骨架补齐覆盖。
	routes := make([]customHandler.OpenAPIRoute, 0, len(h.Routes()))
	for _, rt := range h.Routes() {
		routes = append(routes, customHandler.OpenAPIRoute{Method: rt.Method, Path: rt.Path})
	}
	baseURL := ""
	if cfg := conf.GetGlobalConfig(); cfg != nil {
		baseURL = cfg.Server.BaseURL
	}
	skeleton := customHandler.BuildOpenAPISpec(routes, customHandler.SpecInfo{
		Version: "1.1.0",
		BaseURL: baseURL,
	})
	merged := customHandler.MergeWithIDLSpec(skeleton, openapi.Spec)
	// public 副本：管理面路径物理不可达，公开 spec 不应枚举它们
	if config.IsPublicReplica() {
		merged = stripAdminPlanePaths(merged)
	}
	customHandler.SetOpenAPISpecBytes(merged)

	// 8.5 多副本：public 副本订阅管理端配置变更广播（admin 侧发布钩子在
	// initThriftIDLServices 注入；standalone 两者均不启动）
	if config.IsPublicReplica() {
		startConfigPropagationSubscriber()
	}

	// 首启暴露面告警（2026-10-22）：未初始化且未设令牌时响亮提示——此刻
	// 任何能访问到本实例的人都可以抢先建号。详见 bootstrap/setup_gate.go。
	warnSetupExposure(context.Background())

	logger.Info("Application bootstrap completed successfully")
	return h, nil
}

// publicBaseMiddleware 把请求来源(scheme://host，host 含端口)写入 go ctx 供
// baseurl.Resolve 消费。必须 context.WithValue 包进 go ctx 并经 c.Next 透传：
// 域服务拿到的是 handler 首参 go ctx，c.Set 只落 RequestContext.Keys，service
// 层 ctx.Value 永远读不到（2026-10-12 修复前此推断为死代码，线上实际只有
// base_url 硬配一条路生效；回归守卫见 public_base_middleware_test.go）。
func publicBaseMiddleware() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		if host := string(c.Host()); host != "" {
			scheme := "http"
			if string(c.Request.Header.Peek("X-Forwarded-Proto")) == "https" {
				scheme = "https"
			}
			ctx = context.WithValue(ctx, baseurl.PublicBaseCtxKey, scheme+"://"+host)
		}
		c.Next(ctx)
	}
}

// Cleanup 清理资源（优雅停机编排，oekit core/lifecycle——Manager 原语自 logx
// 迁来，logx 依赖随之移除）：teardown 先取消后台任务根 ctx（拦下一轮），随后
// 各资源按注册序、各自独立超时预算排水——panic 隔离，单个资源卡死不拖垮整体
// 停机；在飞轮次由 bg-tasks 预算内等其跑完。停机失败经 Errors() 落回 zap
// （Manager 内部日志走自身门面，宿主观测以 Errors 为准）。
func Cleanup() {
	logger.Info("Cleaning up resources...")

	mgr := lifecycle.New(15 * time.Second)
	mgr.AddTeardown(stopBackgroundTasks)
	mgr.Add("bg-tasks", drainBackgroundTasks, 10*time.Second)
	// 联邦服务先于数据库停：注销节点（best-effort），停心跳循环（P1 走运行时单点）
	mgr.Add("federation", func(context.Context) error {
		fedRT.stop()
		return nil
	}, 5*time.Second)
	// 直传子进程的停机收口随 Wave B' 一并退役（Box 侧不再 spawn p2pc）。
	mgr.Add("database", func(context.Context) error {
		if database != nil {
			return db.Close()
		}
		return nil
	}, 5*time.Second)
	mgr.Shutdown(context.Background())

	if errs := mgr.Errors(); len(errs) > 0 {
		logger.Error("cleanup incomplete", zap.Errors("errors", errs))
	}

	logger.Sync()
}
