package bootstrap

// 职责：域服务装配——initThriftIDLServices（装配顺序是隐式契约，勿重排）、
// 服务实例全局、bootstrapBaseURL、存储单例与 applyNotifyConfig。

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/oekit/concurrency/async"
	"go.uber.org/zap"
	"gorm.io/gorm"

	adminApp "github.com/pigeonbox/core/app/admin"
	anonymousApp "github.com/pigeonbox/core/app/anonymous"
	chunkApp "github.com/pigeonbox/core/app/chunk"
	configApp "github.com/pigeonbox/core/app/config"
	"github.com/pigeonbox/core/app/federation"
	mcpApp "github.com/pigeonbox/core/app/mcp"
	moderationApp "github.com/pigeonbox/core/app/moderation"
	notifyAppService "github.com/pigeonbox/core/app/notify"
	oidcApp "github.com/pigeonbox/core/app/oidc"
	presignApp "github.com/pigeonbox/core/app/presign"
	previewApp "github.com/pigeonbox/core/app/preview"
	requestApp "github.com/pigeonbox/core/app/request"
	setupApp "github.com/pigeonbox/core/app/setup"
	shareService "github.com/pigeonbox/core/app/share"
	storageApp "github.com/pigeonbox/core/app/storage"
	userService "github.com/pigeonbox/core/app/user"
	"github.com/pigeonbox/core/conf"
	adminGenHandler "github.com/pigeonbox/core/gen/handler/admin"
	chunkHandler "github.com/pigeonbox/core/gen/handler/chunk"
	notifyHandler "github.com/pigeonbox/core/gen/handler/notify"
	p2pGenHandler "github.com/pigeonbox/core/gen/handler/p2p"
	presignHandler "github.com/pigeonbox/core/gen/handler/presign"
	previewHandler "github.com/pigeonbox/core/gen/handler/preview"
	ratelimitHandler "github.com/pigeonbox/core/gen/handler/ratelimit"
	requestgenhandler "github.com/pigeonbox/core/gen/handler/request"
	setupHandler "github.com/pigeonbox/core/gen/handler/setup"
	shareHandler "github.com/pigeonbox/core/gen/handler/share"
	anonHandler "github.com/pigeonbox/core/gen/handler/share_anonymous"
	storageHandler "github.com/pigeonbox/core/gen/handler/storage"
	userHandler "github.com/pigeonbox/core/gen/handler/user"
	"github.com/pigeonbox/core/pkg/auth"
	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/pigeonbox/core/repo/redis"
	"github.com/pigeonbox/core/storage"
	customHandler "github.com/pigeonbox/core/transport/http/handler"
)

// initThriftIDLServices 初始化 thrift IDL 对应的新服务
// 关联 internal/app/ → gen/http/handler/ 各 SetXxx 入口
func initThriftIDLServices(database *gorm.DB) {
	// 0. 单例显式初始化（dao 去全局化：懒单例退役；顺序契约 config 先于 admin）
	configApp.InitDefault(database)
	adminApp.InitDefault(database)

	// 0. 恢复 DB 持久化的运行时存储配置（管理端在线切换的后端类型/s3/webdav）
	restoreRuntimeStorage()
	configApp.Default().RestoreAdminSettings()

	// 0.1 setup 服务（/setup 向导 + /api/config 初始化状态；IDL handler 与
	// handlers_public 共用同一实例）
	setupSvc := setupApp.NewService(database)
	publicSetupSvc = setupSvc
	setupHandler.SetSetupService(setupSvc)

	// 1. notify service（构造注入 database）
	notifyApp := notifyAppService.NewService(database)
	// 1.1 注入定制路由的 notify service
	customHandler.SetNotifyService(notifyApp)
	notifyHandler.SetNotifyService(notifyApp) // gen IDL 路由共用同一实例(此前懒回退裸实例掩盖漏注入)
	// 1.2 供配置广播热重建使用（public 副本收到变更后重挂 Webhook/SMTP 渠道）
	notifySvcInstance = notifyApp

	// 2. presign service（需要 Redis + baseURL + signingKey + share service）
	// baseURL 只认显式配置的对外地址（server.base_url）；未配置时保持空串，
	// 由 share/presign 域按请求来源动态推断（上方中间件注入 ctx）。
	// 禁止回退到 host:port 拼接——server.host 是监听地址（0.0.0.0），
	// 拼进分享链接对外不可达（2026-10-07 真机事故：分享成功弹窗 0.0.0.0 链接）。
	baseURL := config.Server.BaseURL
	// 站点公网地址（2026-10-22）——对外 base 的**动态**来源。
	// 优先级：管理后台 SystemConfig.base.public_url（UI 可改，改完立即生效）
	//         > env/yaml 的 server.base_url（PB_SERVER_BASE_URL）
	//         > 空（baseurl.Resolve 按请求来源推断：Host + X-Forwarded-Proto）。
	// 为什么要有 provider 而不是构造期传死值：server.base_url 只能在 yaml/env
	// 里改，而 NAS/容器打包面的用户不碰 .env —— 分享链接因管理员从内网访问
	// 而带内网 IP（http://10.10.10.3:56789），发给外部用户根本打不开。
	// 接线给 share（分享链接）与 presign（直传回跳地址）两处，同源。
	publicBaseURL := func(ctx context.Context) string {
		if cfg, err := configApp.Default().GetConfig(ctx); err == nil && cfg != nil && cfg.Base != nil {
			if u := strings.TrimSpace(cfg.Base.PublicURL); u != "" {
				return u
			}
		}
		return config.Server.BaseURL
	}
	// presign 签名密钥：conf.PresignSigningKey 单源（PB_PRESIGN_SIGNING_KEY
	// 优先，缺省回退 jwt_secret；chunk 会话令牌同键，取用语义收口一处）
	signingKey := conf.PresignSigningKey()
	presignSvc := presignApp.NewService(database, redis.GetClient(),
		baseURL,
		signingKey)
	presignSvc.SetBaseURLProvider(publicBaseURL)
	presignHandler.SetService(presignSvc)
	// 2.1 注入 share service（Complete 时写分享表）
	shareSvc := shareService.NewService(database, baseURL, getBootstrapStorageService())
	shareSvc.SetBaseURLProvider(publicBaseURL)
	presignHandler.SetShareService(presignShareAdapter{shareSvc})
	// IDL share/chunk 路由此前未注入，走懒加载裸实例（配额/审核等注入缺失），统一共用
	shareHandler.SetShareService(shareSvc)
	chunkHandler.SetShareService(shareSvc)
	// 2.1.1 注入存储服务（presign 直传按当前激活后端落盘）
	presignHandler.SetStorage(getBootstrapStorageService())
	// 2.1.2 注入真预签名直传能力（s3 后端时 Init 签发对象存储直传 URL）
	presignHandler.SetObjectStore(getBootstrapStorageService())
	// 2.2 注入定制路由的 share service
	customHandler.SetShareService(shareSvc)
	// 2.2.1 admin gen handlers（local-files 管理等）共用同一全站实例
	adminGenHandler.SetShareService(shareSvc)
	// 2.3 注入 notify service（取件时给 owner 发通知）
	shareSvc.SetNotifyService(notifyApp) // *Service 已实现 CreateForUserSimple
	// 内容审核钩子（治理 2026-10-03）：moderation.enabled=false 或词表为空时全部放行；
	// clamav.enabled 时文件侧由 clamd 扫描接管（词表管文本、ClamAV 管文件的组合审核器）
	if config.Moderation.Enabled {
		wordMod := moderationApp.NewWordListModerator(
			config.Moderation.BlockedWords, config.Moderation.BlockAction)
		var mod moderationApp.Moderator = wordMod
		if config.Moderation.ClamAV.Enabled {
			clam := moderationApp.NewClamAVModerator(
				config.Moderation.ClamAV.Addr,
				config.Moderation.ClamAV.TimeoutSeconds,
				config.Moderation.ClamAV.MaxScanBytes,
				getBootstrapStorageService())
			mod = moderationApp.NewCombinedModerator(wordMod, clam)
			logger.Info("ClamAV file scanning enabled",
				zap.String("addr", config.Moderation.ClamAV.Addr))
		}
		shareSvc.SetModerator(mod)
	}
	shareSvc.SetFlagEventEmitter(notifyApp) // *Service 已实现 EmitShareFlagged（webhook 未配置时内部短路）
	// 2.3.1/2.3.2 Webhook + SMTP 渠道装配（抽 applyNotifyConfig 供运行时热重建复用）
	// 读**当前快照**（全局配置改为原子换指针后，bootstrap.config 是旧对象）
	if snap := conf.Snapshot(); snap != nil {
		applyNotifyConfig(notifyApp, &snap.Notify)
	}
	// 管理端通知/OIDC 设置保存 → 组件热重建（SystemConfig 新段）
	configApp.Default().SetReconfigureHooks(&configApp.ReconfigureHooks{
		OnNotifyChanged: func(n *conf.NotifyConfig) {
			applyNotifyConfig(notifyApp, n)
		},
		OnOIDCChanged: func(o *conf.OIDCConfig) {
			if o == nil {
				return
			}
			// ⚠️ **copy-on-write**（2026-10-31）：此前是 `config.Security.OIDC = *o`
			// —— 原地改写 bootstrap.config 这个**旧对象**（全局配置改为原子换指针后
			// 它已被换掉）⇒ 改的是没人读的旧快照 ⇒ **OIDC 热生效失效**。
			// 现在写进当前快照的副本，再原子换指针。
			conf.UpdateGlobalConfig(func(next *conf.AppConfiguration) {
				next.Security.OIDC = *o
			})
			customHandler.SetOIDCService(oidcApp.NewService(database, oidcApp.Config{
				Enabled:          o.Enabled,
				Issuer:           o.Issuer,
				ClientID:         o.ClientID,
				ClientSecret:     o.ClientSecret,
				Scopes:           o.Scopes,
				FrontendCallback: o.FrontendCallback,
			}))
		},
		OnP2PFederationChanged: func(f *conf.FederationConfig) error {
			if f == nil {
				return nil
			}
			// 错误必须透传到 admin（断链响亮）：apply 失败=联邦维持原状，
			// 配置值与运行态分叉不可静默（评审 P1-1/P1-2）
			return fedRT.apply(*f)
		},
		OnOverlayApplied: func() { ReapplyEnvOverrides() },
	})
	// 统一配置优先级（yaml < DB < env，2026-10-15）：首次 RestoreAdminSettings
	// 跑在钩子注册之前（上方第 0 步），此处补一次 env 重施对齐存储段同款规则
	ReapplyEnvOverrides()
	// 2.4 注入 user service：上传统计（此前从未接线，用户统计恒为 0）
	// + 存储配额强制检查（user_quota / 用户级 max_storage_quota）
	userSvc := userService.NewService(database)
	userSvc.SetDefaultsProvider(adminDefaultsAdapter{})
	shareSvc.SetUserService(userSvc)
	shareSvc.SetQuotaChecker(userSvc)
	// user 面 handler 同实例注入：注册开关/配额默认值经 DefaultsProvider 桥到
	// admin 持久化配置（此前 handler 直连 admin 包级单例，跨面依赖绕过装配点）
	userHandler.SetUserService(userSvc)

	// 2.4.1 寄件码/反向收件服务（P2）：经 ShareGateway 适配器依赖 share（消跨域 import）
	// 双注入：gen/handler/request（IDL 化四端点）+ customHandler（访客投递重管道桥接）
	requestSvcInstance = requestApp.NewService(database, requestShareGateway{shareSvc}, notifyApp)
	customHandler.SetRequestService(requestSvcInstance)
	requestgenhandler.SetRequestService(requestSvcInstance)

	// 2.4.2 P2P 联邦（spec 2026-10-14 §6）：装配收口 fedRT——启动种子（env/yaml，
	// RestoreAdminSettings 已 overlay DB 值）与 admin 后台热切换同一条路径。
	// 初始化失败降级为非联邦模式；resolve 代理路由恒注册（customizedRegister），
	// 未启用时由 handler 返回 available:false。
	fedRT.init(config.App.Name, federation.NewNode,
		func(svc *federation.Node) {
			shareSvc.SetFederationNotifier(svc)
			// admin 删除路径独立于 share 域，须单独挂钩（管理端删除即联邦撤销）
			adminApp.Default().SetFederationNotifier(svc)
			customHandler.SetFederationService(svc)
			// MCP 在后续相位才创建；运行时热切换须重挂（初装配由其相位自取 instance()）
			if mcpService != nil {
				mcpService.SetFederationService(mcpFederationAdapter{svc: svc})
			}
		},
		func() {
			// 非类型化 nil：typed-nil 传进接口字段非 nil，调用点判空失效（历史坑）
			shareSvc.SetFederationNotifier(nil)
			adminApp.Default().SetFederationNotifier(nil)
			customHandler.SetFederationService(nil)
			if mcpService != nil {
				mcpService.SetFederationService(mcpFederationAdapter{svc: nil})
			}
		},
		// 读**当前快照**的部署形态（部署模式运行期不变，但 bootstrap.config
		// 已是旧对象 ⇒ 统一走当前快照，避免"读旧值"的隐患）
		func() bool { return conf.Snapshot().IsStandalone() }, // 部署形态运行期不变
	)
	if err := fedRT.apply(config.Federation); err != nil {
		// 启动期失败=以非联邦模式运行（管理员无在线会话可报，响亮落日志；
		// 在线保存路径经 ReconfigureHooks 透传给管理员）
		logger.Error("federation 启动装配失败（以非联邦模式运行）", zap.Error(err))
	}

	// 2.4.2.1 联邦信封管理实例（收件箱 + 节点名录；联邦域，2026-10-22 由直传包
	// 构造提升到装配层）
	//
	// 提升原因：此前两个实例在 p2px.NewService 内部构造——删掉直传编排层时联邦
	// 信封能力会跟着一起消失（Wave B 的坑）。现在由装配层独立持有；Wave B'
	// 直传编排层整体退役后，这两个实例是联邦面的**唯一**持有者。
	//
	// 无条件构造：收件箱/名录与「联邦运行时是否启用」无关——收件箱是跨实例交接
	// 的凭证与状态机（公开收件端点恒注册），名录可见性只取决于 registry 可达。
	inviteMailbox := federation.NewInviteMailbox(nil)
	peerDirectory := federation.NewPeerDirectory(
		// 注册中心基址来源（2026-10-22 L4 语义纠偏 + Wave B' 收口）：联邦段
		// 是联邦注册中心（p2pd）的**唯一**真相源，与联邦运行时的注册/心跳同一
		// 注册中心。
		//
		// 直传段（p2p.direct.registry_url）是搬迁前的历史来源，Wave B' 起该段
		// 已整体退役（存量值被忽略并有启动提示）——故不再作兜底。
		//
		// 读 SystemConfig.p2p.federation 而非全局 conf.Federation：前者是
		// overlay（DB 覆盖 yaml）+ env 恒覆盖之后的**生效值镜像**（与联邦
		// 运行时所用的 config.Federation 同源），且由配置域加锁保护——
		// 直读全局 conf 会与 overlay 的原地改写构成数据竞争。
		func(ctx context.Context) (string, bool) {
			cfg, err := configApp.Default().GetConfig(ctx)
			if err != nil || cfg == nil || cfg.P2P == nil || cfg.P2P.Federation == nil {
				return "", false
			}
			u := strings.TrimSpace(cfg.P2P.Federation.RegistryURL)
			if u == "" {
				return "", false
			}
			return u, true
		})

	// 2.4.3 Wave B'（2026-10-22）：纯联邦端点（invites / invites/accept /
	// invite-push / directory）直接取联邦域实例，**不再经直传包** —— 删直传包
	// 时联邦不受影响。设备直传 supervisor 本身已整体退役（Box 侧不再编排直传，
	// 直传由独立应用 PigeonBeam 承担）。
	p2pGenHandler.SetFederationServices(inviteMailbox, peerDirectory)
	p2pGenHandler.SetInviteNotifier(notifySvcInstance.EmitP2pInvite)
	p2pGenHandler.SetAuditWriter(adminApp.Default().LogOperation)

	// 3. anonymous service（需要 Redis）
	anonHandler.SetService(anonymousApp.NewService(redis.GetClient(), database))

	// 4.5 取件码铸造接线（2026-10-07）：share 域经窄接口调用 anonymous 域，
	// 文件分享出码时同步铸 6 位取件码（须共用同一 anon 实例，Redis/内存 KV 才一致）
	shareSvc.SetPickupMinter(anonHandler.CurrentService())
	// 4.6 匿名取件码直传绑定（2026-10-07）：presign Complete 回填 /anonymous/generate
	// 的占位记录（同样必须共用同一 anon 实例）
	presignHandler.SetPickupBinder(anonHandler.CurrentService())

	// 4. ratelimit service（直接用 default limiter）
	ratelimitHandler.SetLimiter(middleware.GetDefaultRateLimiter())

	// 4.5 失败锁定器 + JWT 注销黑名单（Redis 可用时共享，否则内存兜底）
	middleware.InitDefaultLockout(redis.GetClient())
	auth.SetBlacklistRedis(redis.GetClient())

	// 4.6 管理端 admin service（全站唯一实例 Default()：system_configs 为单行
	// JSON 读改写语义，多实例各自缓存内存副本会互相覆盖——2026-10-03 事故；
	// 路由增强/存储配置持久化/MCP/定时清理与 gen admin handler 共用同一实例）
	adminSvc := adminApp.Default()

	// 4.6.1 storage 管理 service（连接测试/在线切换：认证级 Probe + 热重载 + 持久化）
	storageSvc := storageApp.NewService()
	storageSvc.SetRuntime(getBootstrapStorageService())
	storageSvc.SetPersister(configApp.Default()) // RuntimePersister=config 域(运行时存储段唯一写者)
	storageSvc.SetObjectLister(adminSvc)         // ObjectLister=admin 域(存储域不直接碰 DB)
	storageHandler.SetService(storageSvc)
	storageHandler.SetAdminStats(adminSvc)

	// 4.7 管理端增强服务注入（用户 CRUD/文件管理/富统计）：
	//   - adminSvc/userSvc → gen/handler/admin（2026-10-10 IDL 化 20 端点）
	//   - storage → transport/http/handler（share 多文件/寄件码上传落盘依赖面）
	adminGenHandler.SetManageServices(adminSvc, userSvc)
	customHandler.SetManageStorage(getBootstrapStorageService())

	// 5. 自动迁移 notify 表 + file_codes viewer 字段
	//    （迁移只在 standalone/admin：public 副本的 database.auto_migrate 已被
	//    applyDeploymentConstraints 强制关闭，这里同步跳过，防多副本迁移竞态）
	if config.Database.AutoMigrate {
		if err := database.AutoMigrate(&model.Notify{}); err != nil {
			logger.Error("Failed to migrate notify table", zap.Error(err))
		} else {
			logger.Info("Notify table migrated")
		}
		if err := database.AutoMigrate(&model.FileCode{}); err != nil {
			logger.Error("Failed to migrate file_codes table", zap.Error(err))
		} else {
			logger.Info("FileCode table migrated (viewer fields added)")
		}
		// 多文件子表（P0 多文件）：1 分享 ↔ N 文件
		if err := database.AutoMigrate(&model.FileCodeFile{}); err != nil {
			logger.Error("Failed to migrate file_code_files table", zap.Error(err))
		}
		// 寄件码表（P2 反向收件）
		if err := database.AutoMigrate(&model.FileRequest{}); err != nil {
			logger.Error("Failed to migrate file_requests table", zap.Error(err))
		}
		// 取件计次表（2026-10-24）：一个下载令牌至多扣 1 次取件次数
		if err := database.AutoMigrate(&model.PickupCharge{}); err != nil {
			logger.Error("Failed to migrate pickup_charges table", zap.Error(err))
		}
	}

	// 6. 注入 storage 到 admin service（过期清理删物理文件；app 服务直注，
	//    gen handler 不再作为存储注入的透传层）
	bootstrapStorage := getBootstrapStorageService()
	adminSvc.SetStorage(bootstrapStorage)

	// 6.5 统一存储实例注入（消除懒加载单例路径基分歧：分片合并写入
	// data/uploads/<rel>，下载却找 data/uploads/uploads/<rel>）。
	// chunk：存储随 app service 注入，gen handler 只持服务句柄；
	// share：存储由 app service 持有（shareSvc 构造时注入），不经 gen handler；
	// preview：业务流已下沉 app/preview，存储随 service 注入。
	chunkSvc := chunkApp.NewService(database)
	chunkSvc.SetStorage(bootstrapStorage)
	chunkSvc.SetUploadSizeCap(userSvc) // 登录用户单次上传上限预检（chunk init fail-fast，2026-10-19 事故）
	chunkHandler.SetChunkService(chunkSvc)
	previewSvcInstance := previewApp.NewService(database, bootstrapStorage)
	previewHandler.SetService(previewSvcInstance)
	customHandler.SetMultiChunkService(chunkSvc) // 多文件绑定 chunk 会话查询(此前每调自建掩盖漏注入)

	// 6.5 MCP server（AI 客户端集成）：统计/维护走全站唯一 admin 实例，
	//     分享创建走 share service（复用配额/审核链路）、文件下载走全站存储实例、
	//     联邦状态/路由走 federation 实例（未启用为 nil 适配器）、
	//     寄件码/预览走 request/preview 实例——经窄接口适配器注入
	if config.MCP.Enabled {
		mcpService = mcpApp.NewService(database, config.App.Version)
		mcpService.SetAdminService(mcpAdminAdapter{adminSvc})
		mcpService.SetShareService(mcpShareAdapter{shareSvc})
		mcpService.SetStorageService(bootstrapStorage)
		mcpService.SetFederationService(mcpFederationAdapter{svc: fedRT.instance()})
		// 联邦三工具走联邦域实例（在线名录/待收邀请/认领信封）；直传工具已随
		// Wave B' 整体退役（Box 侧不再编排直传）。
		mcpService.SetP2pService(mcpP2pAdapter{
			mailbox:   inviteMailbox,
			directory: peerDirectory,
		})
		mcpService.SetRequestService(mcpRequestAdapter{requestSvcInstance})
		mcpService.SetPreviewService(mcpPreviewAdapter{previewSvcInstance})
		mcpService.SetMaxFileSize(config.MCP.MaxFileSize)
		mcpService.SetBaseURL(baseURL)
	}

	// 7. 启动过期文件定时清理（默认每小时，删 DB 记录 + 物理文件），
	//    复用全站唯一 admin 实例（storage 注入幂等）。
	//    后台任务全局只跑一份：仅 standalone/admin 执行（public 副本不跑，
	//    否则 N 副本重复清理/对账）。
	adminSvc.SetStorage(bootstrapStorage)
	if config.ServesAdminPlane() {
		// 后台常驻协程统一 GoSafe 起跑（panic 走全局 zap handler，不静默打死）
		async.GoSafe(func() { startExpiredFileCleanup(adminSvc) })
		// API Key 临期站内通知（波次3）：6h 周期，提前 7 天提醒属主
		async.GoSafe(startAPIKeyExpiryNotify)
		// 存储对账 + 日志保留（治理 2026-10-03）：24h 周期，启动 10 分钟后首跑
		async.GoSafe(startMaintenanceJanitor)
	}

	// 8. 多副本：admin 实例持久化配置后发布变更广播（public 副本订阅热应用）
	if config.IsAdminReplica() {
		configApp.Default().SetOnConfigPersisted(publishConfigChanged)
	}
}

// mcpService MCP server 实例（initThriftIDLServices 装配，customizedRegister 挂路由）
var mcpService *mcpApp.Service

// notifySvcInstance 通知服务实例（initThriftIDLServices 装配；配置广播热重建用）
var notifySvcInstance *notifyAppService.Service

// requestSvcInstance 寄件码服务实例（initThriftIDLServices 装配）
var requestSvcInstance *requestApp.Service

// bootstrapBaseURL 对外基础地址（server.base_url 优先，否则 host:port）
func bootstrapBaseURL() string {
	if config.Server.BaseURL != "" {
		return config.Server.BaseURL
	}
	return fmt.Sprintf("http://%s:%d", config.Server.Host, config.Server.Port)
}

// bootstrapStorage 单例：share/chunk/presign/admin/清理共用同一实例，
// 管理端在线切换存储（Reload）才能对全部读写链路生效。
var (
	bootstrapStorageOnce sync.Once
	bootstrapStorageSvc  *storage.StorageService
)

// applyNotifyConfig 通知渠道装配：Webhook + SMTP mailer（host 空 = 卸载 mailer）。
// 启动装配与运行时热重建（管理端通知设置保存）共用。
func applyNotifyConfig(target *notifyAppService.Service, cfg *conf.NotifyConfig) {
	if cfg == nil {
		return
	}
	target.SetWebhookURL(cfg.WebhookURL)
	if cfg.SMTP.Host != "" {
		target.SetMailer(notifyAppService.NewSMTPMailer(
			cfg.SMTP.Host,
			cfg.SMTP.Port,
			cfg.SMTP.Username,
			cfg.SMTP.Password,
			cfg.SMTP.From,
			userEmailLookup{},
		))
		logger.Info("SMTP mail notifications enabled", zap.String("host", cfg.SMTP.Host))
	} else {
		target.SetMailer(nil)
	}
}

// getBootstrapStorageService bootstrap 用的 storage 单例。
// 配置来自 conf（DB 持久化的 runtime_storage 已由 restoreRuntimeStorage 恢复进 conf）；
// 远端驱动构造失败时记录错误并降级 local（EffectiveType/InitError 可查真相）。
func getBootstrapStorageService() *storage.StorageService {
	bootstrapStorageOnce.Do(func() {
		cfg := storage.ConfigFromConf(&config.Storage, bootstrapBaseURL())
		svc, err := storage.NewStorageServiceE(cfg)
		if err != nil {
			logger.Error("remote storage backend init failed, fallback to local",
				zap.String("type", string(cfg.Type)), zap.Error(err))
			svc = storage.NewStorageService(cfg)
		} else if t := svc.EffectiveType(); t != storage.StorageTypeLocal {
			logger.Info("remote storage backend enabled", zap.String("type", string(t)))
		}
		// 历史双层布局上提（2026-10-26 兼容层退役）：storage_path 曾带 uploads 段，
		// 而存储相对路径本身又以 uploads/ 开头 ⇒ 物理路径多一层。读路径的回退分支
		// 已删 ⇒ 迁移必须先于任何文件读取完成，故在存储单例构造处就地执行。
		if svc.EffectiveType() == storage.StorageTypeLocal {
			if moved, skipped, merr := storage.MigrateLegacyLayout(cfg.DataPath); merr != nil {
				logger.Error("legacy storage layout migration failed", zap.Error(merr))
			} else if moved > 0 || skipped > 0 {
				logger.Info("legacy storage layout migration done",
					zap.Int("moved", moved), zap.Int("skipped", skipped))
			}
		}
		bootstrapStorageSvc = svc
	})
	return bootstrapStorageSvc
}
