package conf

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// globalConfig 全局配置快照（原子持有）。
//
// 设计权衡：bootstrap 启动时 SetGlobalConfig 注入，各处用 GetGlobalConfig 读取。
// 采用全局单例避免配置在 service/handler 间层层透传。未来若迁移到 DI 容器，
// 可改为构造注入。
//
// # ⚠️ 运行期**会**改写（原注释写「运行期不可变（无写并发）」——与实现矛盾，
// 2026-10-31 更正）
//
// app/config 的「新段热应用」（applySystemConfigOverlayLocked）会在**管理员保存
// 配置时原地改写**本对象的字段（UI / Upload / Download / Notify /
// Security.OIDC / Security.APIToken / Federation……每段都是整段结构体赋值）。
//
// 而 GetGlobalConfig() 有 **45 处**读取点（上传白名单、magic check、下载过期、
// SMTP、OIDC、匿名配额……），分布在请求路径上，**无锁读**这些字段。
//
// ⇒ 原地改写 + 无锁读 = **结构体字段的数据竞争**：读者可能看到**撕裂的字段
// 组合**（如 Upload.ExUpload 已是新值、白名单还是旧值）⇒ 闸口按半新半旧的
// 配置判定。
//
// # 修法：copy-on-write + 原子换指针
//
//   - 写侧：UpdateGlobalConfig(mutator) —— **复制一份**当前快照，在副本上改，
//     再原子换指针。读者要么看到完整旧快照，要么看到完整新快照，**永不撕裂**；
//   - 读侧：GetGlobalConfig() 一次 Load()，拿到一致的快照指针。
//
// ⇒ 全局配置在运行期变成**不可变快照**，热生效靠换指针而不是改字段。
var globalConfig atomic.Pointer[AppConfiguration]

// AppConfiguration 完整应用配置
type AppConfiguration struct {
	Server        ServerConfig        `mapstructure:"server"`
	Database      DatabaseConfig      `mapstructure:"database"`
	Redis         RedisConfig         `mapstructure:"redis"`
	Log           LogConfig           `mapstructure:"log"`
	App           AppConfig           `mapstructure:"app"`
	User          UserConfig          `mapstructure:"user"`
	Upload        UploadConfig        `mapstructure:"upload"`
	Download      DownloadConfig      `mapstructure:"download"`
	Storage       StorageConfig       `mapstructure:"storage"`
	UI            UIConfig            `mapstructure:"ui"`
	Observability ObservabilityConfig `mapstructure:"observability"`
	Security      SecurityConfig      `mapstructure:"security"`
	RateLimit     RateLimitSettings   `mapstructure:"rate_limit"`
	Notify        NotifyConfig        `mapstructure:"notify"`
	MCP           MCPConfig           `mapstructure:"mcp"`
	Moderation    ModerationConfig    `mapstructure:"moderation"`
	Admin         AdminConfig         `mapstructure:"admin"`
	Federation    FederationConfig    `mapstructure:"federation"`
	Deployment    DeploymentConfig    `mapstructure:"deployment"`
}

// 部署模式取值（deployment.mode / env PB_DEPLOY_MODE）。
// 详见 docs/specs/2026-10-06-multi-replica-deployment-modes.md。
const (
	// DeployModeStandalone 单进程全功能（默认，即历史形态：全部路由 + 后台任务 + 迁移）。
	DeployModeStandalone = "standalone"
	// DeployModePublic 公开面副本（可多副本横向扩容）：只注册公开路由，
	// 不跑后台任务/迁移，只读 system_configs 并订阅管理端变更广播。
	DeployModePublic = "public"
	// DeployModeAdmin 管理面单实例：admin/mcp/setup 等管理路由 + 后台任务 +
	// DB 迁移 + system_configs 唯一写者（变更后 Redis 广播）。
	DeployModeAdmin = "admin"
)

// DeploymentConfig 部署形态（多副本拆分，2026-10-06）。
// 同一镜像三种运行模式：public×N + admin×1 组成多副本拓扑；
// 单机部署保持默认 standalone，行为与历史版本完全一致。
type DeploymentConfig struct {
	// Mode standalone(默认) | public | admin。env: PB_DEPLOY_MODE
	Mode string `mapstructure:"mode"`
}

// NormalizedDeploymentMode 归一化部署模式（空值回退 standalone；
// 非法值由 bootstrap 的 applyDeploymentConstraints fail-fast 拒绝，此处不做校验）。
func (c *AppConfiguration) NormalizedDeploymentMode() string {
	if c == nil || c.Deployment.Mode == "" {
		return DeployModeStandalone
	}
	return c.Deployment.Mode
}

// IsStandalone 是否单机全功能模式（默认）。
func (c *AppConfiguration) IsStandalone() bool {
	return c.NormalizedDeploymentMode() == DeployModeStandalone
}

// IsPublicReplica 是否公开面副本（public 模式）。
func (c *AppConfiguration) IsPublicReplica() bool {
	return c.NormalizedDeploymentMode() == DeployModePublic
}

// IsAdminReplica 是否管理面单实例（admin 模式）。
func (c *AppConfiguration) IsAdminReplica() bool {
	return c.NormalizedDeploymentMode() == DeployModeAdmin
}

// ServesPublicPlane 是否注册公开面（standalone 与 public）。
func (c *AppConfiguration) ServesPublicPlane() bool {
	return !c.IsAdminReplica()
}

// ServesAdminPlane 是否注册管理面 + 后台任务 + 迁移（standalone 与 admin）。
func (c *AppConfiguration) ServesAdminPlane() bool {
	return !c.IsPublicReplica()
}

// FederationAllowedInMode 部署模式是否允许联邦。
//
// 多副本拓扑（public / admin）下联邦**架构上不可用**：联邦节点身份是**进程级**
// Ed25519 密钥（PB_FEDERATION_NODE_KEY_PATH），多副本各持各的身份 ⇒
// 注册/解析语义未定义（2026-10-30 拍板：暂不支持）⇒ 一律关闭。
//
// ⚠️ 这是**部署模式派生的硬约束**，不是配置项：管理台保存的联邦开关
// （DB 里 system_configs 的 p2p.federation）是**提交意图**，不是生效值——
// 它**不得**把多副本下的联邦打开。
//
// # 为什么放在 conf（单一真相源）
//
// 同一约束有两个触发点，口径必须一致，否则会漂：
//
//  1. bootstrap 的 applyDeploymentConstraints（**启动时**降级）；
//  2. app/config 的 applySystemConfigOverlayLocked（**运行期**热应用；
//     管理台保存 / 多副本配置广播都会走到）。
//
// 第 2 点曾经漏了这条约束 ⇒ overlay 的整段赋值（next.Federation = *sc.P2P.Federation）
// 把启动时的降级**复活** ⇒ 多副本下 /api/config 报 federationEnabled=true
// ⇒ 前端承诺了架构上不可用的跨站取件。两处都必须走本函数。
func FederationAllowedInMode(mode string) bool {
	// 空模式按 standalone 归一（与 NormalizedDeploymentMode 同一约定）——
	// 否则调用方传原始 mode（空串）时会与传归一值得到**相反**的结论。
	if mode == "" {
		mode = DeployModeStandalone
	}
	return mode == DeployModeStandalone
}

// FederationConfig P2P 联邦接入（M2；默认关闭。对端服务：github.com/pigeonbox/p2p）。
// 启用后本站注册进联邦注册中心，口令分享跨站可达（文件仍从本站直出，
// 注册中心不落盘不见明文）。详见 docs/specs/2026-10-04-p2p-registry-service-design.md。
type FederationConfig struct {
	// Enabled 总开关（默认 false）。env: PB_FEDERATION_ENABLED
	Enabled bool `mapstructure:"enabled" json:"enabled"`
	// RegistryURL 联邦注册中心基址，如 http://p2p:12346；支持逗号分隔多主备
	// （写路径全推、读路径依次 failover）。env: PB_FEDERATION_REGISTRY_URL
	RegistryURL string `mapstructure:"registry_url" json:"registry_url"`
	// PublicURL 本站对外可达基址（公告给取件方直连下载用；必须公网/局域网可达，
	// 一般与 server.base_url 一致）。env: PB_FEDERATION_PUBLIC_URL
	PublicURL string `mapstructure:"public_url" json:"public_url"`
	// NodeKeyPath 节点身份密钥（Ed25519 seed hex, 0600），首次启动自动生成。
	// 缺失/损坏会重新生成=联邦身份更换。env: PB_FEDERATION_NODE_KEY_PATH
	NodeKeyPath string `mapstructure:"node_key_path" json:"node_key_path"`
	// AnnounceMinEntropyBits 口令公告熵门槛（位），低于此熵的口令不出站
	// （6 位数字取件码默认只在本站有效，防 registry 侧枚举）。0=默认 40。
	// json 标签随 SystemConfig.p2p 段在线读写（2026-10-14 对称节点 P1）。
	AnnounceMinEntropyBits int `mapstructure:"announce_min_entropy_bits" json:"announce_min_entropy_bits"`
	// DeliveryPeers 可投递对端（联邦前台化，2026-11-01；跨站投递）。
	//
	// 双向登记才允许投递：A 柜登记 B ⇒ A 能往 B 投；B 柜登记 A ⇒ B 才认 A
	// 的铸槽位请求（口令即权限）。
	//
	// ⚠️ 段内含**共享口令明文** ⇒ 管理台不回显（只显示是否已配 + 名称）；
	// json 标签随 SystemConfig.p2p 段在线读写（同 AnnounceMinEntropyBits）。
	DeliveryPeers []FederationDeliveryPeer `mapstructure:"delivery_peers" json:"delivery_peers"`
}

// FederationDeliveryPeer 可投递对端（联邦前台化，2026-11-01）。
//
// 寻址口径 = **对方站点基址 URL**（用户 2026-11-01 拍板）：访客在 A 柜前台
// 填 B 柜的基址，A 柜只认**管理员登记过的**基址 ⇒ 误投面与 SSRF 面同时收窄
// （出站 URL 只来自 admin 配置，不是访客任意输入）。
//
// 信任模型沿用现有联邦的「口令 bearer」（口令即权限，同手工交接信任级）⇒
// 不引入新的认证机制，也**不复用**管理员口令。
type FederationDeliveryPeer struct {
	// URL 对方柜基址（如 http://10.10.30.253:12345）。scheme 限 http/https。
	URL string `mapstructure:"url" json:"url"`
	// Token 共享口令 bearer（A 柜调 B 柜铸槽位时带上；B 柜比对登记对端的口令）。
	Token string `mapstructure:"token" json:"token"`
	// Name 前台展示名（如「张三的柜子」；访客在投递结果里看到）。
	Name string `mapstructure:"name" json:"name"`
	// AllowPrivate 允许该对端落在私网（自托管常态：对方柜常在局域网）。
	//
	// ⚠️ **按对端粒度**开，**不动**全局 security.ssrf.allow_private_networks
	//（全局开关一开 = 所有出站点位都放行私网 ⇒ 面太大）。
	AllowPrivate bool `mapstructure:"allow_private" json:"allow_private"`
}

// MCPConfig Model Context Protocol server（AI 客户端集成；上游没有的差异化能力）。
// 端点 POST /api/v1/mcp（Streamable HTTP / JSON-RPC 2.0），默认挂管理员 JWT 认证。
type MCPConfig struct {
	// Enabled 默认 true（认证已强制，无暴露风险）。env: PB_MCP_ENABLED
	Enabled bool `mapstructure:"enabled"`
	// AllowGuest 访客快速分享（2026-10-12，对标上游 v2.8 MCP quick share 并超出）：
	// true 时 /api/v1/mcp 不再强制管理员 JWT——游客仅 share_text/share_file
	// （与网页匿名寄件同链路、同限流、同审核/白名单），登录用户 += create_request_code/
	// preview_share，管理员 = 全量工具。默认 false（维持管理员专属）。
	// env: PB_MCP_ALLOW_GUEST
	AllowGuest bool `mapstructure:"allow_guest"`
	// MaxFileSize MCP 单文件上传/下载上限（字节）。默认 6MB——base64 膨胀 4/3 后
	// 约 8MB，低于默认请求体上限（max(10MB, upload.max_file_size)）；
	// 调大超过请求体上限时须同步调大 upload.max_file_size。env: PB_MCP_MAX_FILE_SIZE
	MaxFileSize int64 `mapstructure:"max_file_size"`
}

// AdminConfig 管理端运维配置（治理 2026-10-03）
type AdminConfig struct {
	// LogRetentionDays 审计/传输日志保留天数（0 = 永久）。env: PB_ADMIN_LOG_RETENTION_DAYS
	LogRetentionDays int `mapstructure:"log_retention_days"`
	// PickupDetailRetentionDays **取件明细**保留天数（0 = 永久）。
	// env: PB_ADMIN_PICKUP_DETAIL_RETENTION_DAYS
	//
	// 为什么与 LogRetentionDays 分开（2026-10-24）：取件计次记录表同时承担两个角色
	// ——①「同一令牌不重复计次」的去重键（令牌过期后即可丢）；② owner 侧「取件历史」
	// 的逐条明细（IP / 时间 / 交付面）⇒ ②的留存需求**远长于**①。
	// 此前两者共用一个「令牌 TTL + 1h」的清理窗口 ⇒ **owner 看到的明细最多只存在
	// 1 小时**（第二天打开取件历史，明细全空）⇒ 「逐条取件历史」形同虚设。
	PickupDetailRetentionDays int `mapstructure:"pickup_detail_retention_days"`
}

// ModerationConfig 内容审核配置（治理 2026-10-03；默认关闭，词表为空恒放行）
type ModerationConfig struct {
	// Enabled 审核钩子总开关（默认 false；env: PB_MODERATION_ENABLED）
	Enabled bool `mapstructure:"enabled"`
	// BlockedWords 敏感词表（子串匹配、大小写不敏感；env: PB_MODERATION_BLOCKED_WORDS，逗号分隔）
	BlockedWords []string `mapstructure:"blocked_words"`
	// BlockAction 命中处置策略：reject（默认，直接拒绝）| pending（建分享后置待审，进管理端队列）
	BlockAction string `mapstructure:"block_action"`
	// ClamAV 文件病毒扫描（clamd INSTREAM；启用后文件侧审核由扫描器接管）
	ClamAV ClamAVConfig `mapstructure:"clamav"`
}

// ClamAVConfig clamd 病毒扫描配置（默认关闭；详见 app/moderation/clamav.go）
type ClamAVConfig struct {
	Enabled        bool   `mapstructure:"enabled"`         // env: PB_MODERATION_CLAMAV_ENABLED
	Addr           string `mapstructure:"addr"`            // 默认 localhost:3310；env: PB_MODERATION_CLAMAV_ADDR
	TimeoutSeconds int    `mapstructure:"timeout_seconds"` // 单文件扫描超时，默认 60
	MaxScanBytes   int64  `mapstructure:"max_scan_bytes"`  // 超过跳过扫描，默认 512MB
}

// NotifyConfig 通知配置（站内信 + 外部 Webhook + SMTP 邮件渠道）
type NotifyConfig struct {
	// WebhookURL 外部推送地址：notify.created 事件以 JSON POST 推送（空 = 禁用）。
	// env: PB_WEBHOOK_URL
	WebhookURL string `mapstructure:"webhook_url" json:"webhook_url"`
	// SMTP 邮件通知（站内信创建后对登记邮箱异步补发；空 host = 禁用）
	SMTP SMTPConfig `mapstructure:"smtp" json:"smtp"`
	// Telegram 推送渠道（bot_token + chat_id 均非空 = 启用；缺省空 = 禁用）
	Telegram TelegramConfig `mapstructure:"telegram" json:"telegram"`
	// Bark 推送渠道（server_url + device_key 均非空 = 启用；缺省空 = 禁用）
	Bark BarkConfig `mapstructure:"bark" json:"bark"`
}

// TelegramConfig Telegram 推送渠道配置（2026-11-01 外部推送渠道扩展）。
// 目标 host 固定 api.telegram.org（不可由用户输入）；缺省空 = 禁用。
type TelegramConfig struct {
	BotToken string `mapstructure:"bot_token" json:"bot_token"` // env: PB_TELEGRAM_BOT_TOKEN
	ChatID   string `mapstructure:"chat_id" json:"chat_id"`     // env: PB_TELEGRAM_CHAT_ID
}

// BarkConfig Bark 推送渠道配置（2026-11-01 外部推送渠道扩展）。
// ServerURL 由用户填（自建 Bark）⇒ 出站前必过 security.ValidateEndpointURL（SSRF）；
// 缺省空 = 禁用。
type BarkConfig struct {
	ServerURL string `mapstructure:"server_url" json:"server_url"` // env: PB_BARK_SERVER_URL
	DeviceKey string `mapstructure:"device_key" json:"device_key"` // env: PB_BARK_DEVICE_KEY
}

// SMTPConfig SMTP 邮件配置（P2；默认禁用）
type SMTPConfig struct {
	Host     string `mapstructure:"host" json:"host"`         // env: PB_SMTP_HOST
	Port     int    `mapstructure:"port" json:"port"`         // 465=隐式 TLS；25/587=STARTTLS；env: PB_SMTP_PORT
	Username string `mapstructure:"username" json:"username"` // env: PB_SMTP_USERNAME
	Password string `mapstructure:"password" json:"password"` // env: PB_SMTP_PASSWORD
	From     string `mapstructure:"from" json:"from"`         // 发件地址，空 = 取 Username；env: PB_SMTP_FROM
}

// SetGlobalConfig 设置全局配置
func SetGlobalConfig(cfg *AppConfiguration) {
	globalConfig.Store(cfg)
}

// GetGlobalConfig 获取全局配置
//
// 返回的是**当前快照**（不可变）：调用方拿到的指针在一次读取里是自洽的，
// 但**不要直接改它的字段**——要用 UpdateGlobalConfig 走 copy-on-write
// （否则会绕过原子换指针，重新引入撕裂读）。
func GetGlobalConfig() *AppConfiguration {
	return globalConfig.Load()
}

// Snapshot 取当前全局配置快照（**推荐入口**）。
//
// 与 GetGlobalConfig() 等价，但语义更明确：返回值是**不可变快照**，
// 调用方要么用它读、要么整段替换后再走 UpdateGlobalConfig，
// **不要直接改它的字段**（那会绕过原子换指针，重新引入撕裂读）。
//
// ⚠️ 取到快照后**不要长期持有**：热更新会换指针 ⇒ 长期持有的快照会过期。
// 需要长期持有就复制一份需要的内容。
func Snapshot() *AppConfiguration {
	return globalConfig.Load()
}

// UpdateGlobalConfig 以 copy-on-write 方式更新全局配置。
//
// mutator 在**当前快照的副本**上修改；返回后原子换指针。
// ⇒ 并发读者要么看到完整旧快照、要么看到完整新快照，**永不看到撕裂的中间态**。
//
// 之所以必须这样：热应用（管理台保存配置）会整段改写 Upload / Download /
// Notify / Security 等大段字段，而 45 处读取点在请求路径上无锁读它们 ⇒
// 原地改写即数据竞争。
//
// mutator 收到的副本是浅拷贝：段内若有引用类型字段（map/slice/指针），
// mutator 若要改它们的**内容**（而非整体替换），需自行深拷贝——
// 整体替换某个段（cfg.Upload = *newUpload）是安全的。
//
// 返回值是**换指针之后**的新快照：调用方若要读生效值，用它而不要再直读
// （避免多一个直读点，也避免读到换指针前的旧值）。
func UpdateGlobalConfig(mutator func(cfg *AppConfiguration)) *AppConfiguration {
	for {
		old := globalConfig.Load()
		if old == nil {
			return nil
		}
		// 浅拷贝一份：段内引用类型字段共享底层，但「整体替换某段」是原子的
		next := *old
		mutator(&next)
		// CAS：期间若有人换过快照，重试（避免丢掉别人的更新）
		if globalConfig.CompareAndSwap(old, &next) {
			return &next
		}
	}
}

// ServerConfig 服务器配置
type ServerConfig struct {
	Host         string `mapstructure:"host"`
	Port         int    `mapstructure:"port"`
	Mode         string `mapstructure:"mode"`     // debug, release, test
	BaseURL      string `mapstructure:"base_url"` // 对外可访问的基础 URL（用于生成分享链接等）
	ReadTimeout  int    `mapstructure:"read_timeout"`
	WriteTimeout int    `mapstructure:"write_timeout"`
}

// DatabaseConfig 数据库配置
type DatabaseConfig struct {
	Driver      string `mapstructure:"driver"` // sqlite, mysql, postgres
	DBName      string `mapstructure:"db_name"`
	Host        string `mapstructure:"host"`
	Port        int    `mapstructure:"port"`
	User        string `mapstructure:"user"`
	Password    string `mapstructure:"password"`
	AutoMigrate bool   `mapstructure:"auto_migrate"` // true=GORM AutoMigrate(开发友好,默认); false=版本化迁移(migrations/)
	Migrate     bool   `mapstructure:"migrate"`      // 启动时执行版本化迁移(企业级)。与 AutoMigrate 可共存：先 migrate 后 AutoMigrate 兜底
}

// RedisConfig Redis配置
type RedisConfig struct {
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
}

// Addr 返回 Redis 地址
func (c *RedisConfig) Addr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

// PublicBaseURL 对外基础地址：base_url 优先，否则 host:port。
// fallback 供配置未加载（测试）兜底。修复：直传/分片通道此前在
// base_url 未配置时回落到写死的 localhost:12345 常量，反代/容器部署
// 下 API 返回的分享链接不可用。
func (c *ServerConfig) PublicBaseURL(fallback string) string {
	if c != nil {
		if c.BaseURL != "" {
			return c.BaseURL
		}
		if c.Port > 0 {
			return fmt.Sprintf("http://%s:%d", c.Host, c.Port)
		}
	}
	return fallback
}

// LogConfig 日志配置
type LogConfig struct {
	Level      string `mapstructure:"level"`
	Filename   string `mapstructure:"filename"`
	MaxSize    int    `mapstructure:"max_size"`
	MaxBackups int    `mapstructure:"max_backups"`
	MaxAge     int    `mapstructure:"max_age"`
	Compress   bool   `mapstructure:"compress"`
}

// AppConfig 应用配置
type AppConfig struct {
	Name        string `mapstructure:"name"`
	Version     string `mapstructure:"version"`
	Description string `mapstructure:"description"`
	DataPath    string `mapstructure:"datapath"`
	Production  bool   `mapstructure:"production"`
}

// UserConfig 用户配置
type UserConfig struct {
	AllowUserRegistration bool   `mapstructure:"allow_user_registration"`
	UserUploadSize        int64  `mapstructure:"user_upload_size"`
	UserStorageQuota      int64  `mapstructure:"user_storage_quota"`
	SessionExpiryHours    int    `mapstructure:"session_expiry_hours"`
	MaxSessionsPerUser    int    `mapstructure:"max_sessions_per_user"`
	JWTSecret             string `mapstructure:"jwt_secret"`
}

// UploadConfig 上传配置
type UploadConfig struct {
	OpenUpload   bool  `mapstructure:"open_upload" json:"open_upload"`
	UploadSize   int64 `mapstructure:"upload_size" json:"upload_size"`
	EnableChunk  bool  `mapstructure:"enable_chunk" json:"enable_chunk"`
	ChunkSize    int64 `mapstructure:"chunk_size" json:"chunk_size"`
	RequireLogin bool  `mapstructure:"require_login" json:"require_login"`
	// TextMaxBytes 文本分享大小上限（字节）。<=0 时用默认 222KB（对齐上游）。
	TextMaxBytes int64 `mapstructure:"text_max_bytes" json:"text_max_bytes"`
	// AllowedExtensions 扩展名白名单（如 [".jpg",".png",".pdf"]）。
	// 非空时白名单优先：未命中的扩展名直接拒绝；空 = 黑名单模式。
	AllowedExtensions []string `mapstructure:"allowed_extensions" json:"allowed_extensions"`
	// BlockedExtensions 扩展名黑名单（如 [".exe",".bat"]）。
	// 非空时覆盖内置默认黑名单；空 = 使用内置默认（可执行文件类）。
	BlockedExtensions []string `mapstructure:"blocked_extensions" json:"blocked_extensions"`
	// EnableMagicCheck 是否启用魔数校验（默认 true）。
	EnableMagicCheck bool `mapstructure:"enable_magic_check" json:"enable_magic_check"`
	// AllowedExpireStyles 允许用户选择的过期样式
	// （minute/hour/day/week/month/year/forever/count —— count =「按取件次数」，
	// 2026-10-22 补入：此前注释漏了它，合法集合以 pkg/utils.AllExpireStyles 为准）。
	// 空 = 全部允许；管理员可裁剪（对标上游白名单裁剪能力）。
	AllowedExpireStyles []string `mapstructure:"allowed_expire_styles" json:"allowed_expire_styles"`
	// MaxSaveSecondsCap 全局过期时间上限（秒），0 = 不限（对标上游 max_save_seconds）。
	MaxSaveSecondsCap int64 `mapstructure:"max_save_seconds_cap" json:"max_save_seconds_cap"`
	// MaxFileSize 单个文件大小上限（字节），适用于分片/预签名/匿名登记等
	// "整文件"语义的通道；0 = 不限。upload_size 是"单请求体"上限，两者语义不同
	// （回归：分片 init 曾误用 upload_size，48MB 文件被 10MB 单请求限制拦截）。
	MaxFileSize int64 `mapstructure:"max_file_size" json:"max_file_size"`
	// AnonymousDailyCount 匿名上传 per-IP 日配额（次数），0 = 不限。
	// 仅约束匿名请求（登录用户走存储配额）；Redis 可用时多实例共享计数。
	AnonymousDailyCount int64 `mapstructure:"anonymous_daily_count" json:"anonymous_daily_count"`
	// AnonymousDailyBytes 匿名上传 per-IP 日配额（字节），0 = 不限。
	AnonymousDailyBytes int64 `mapstructure:"anonymous_daily_bytes" json:"anonymous_daily_bytes"`
	// LocalImport NAS 本地文件免上传导入（P3：服务器本地白名单目录内的文件
	// 直接登记为分享，服务端拷贝入存储；默认关闭）
	LocalImport LocalImportConfig `mapstructure:"local_import" json:"local_import"`
}

// LocalImportConfig 本地文件导入配置
type LocalImportConfig struct {
	Enabled bool     `mapstructure:"enabled" json:"enabled"` // env: PB_LOCAL_IMPORT_ENABLED
	Roots   []string `mapstructure:"roots" json:"roots"`     // 允许导入的绝对目录白名单；env: PB_LOCAL_IMPORT_ROOTS（逗号分隔）
}

// DownloadConfig 下载配置
type DownloadConfig struct {
	DownloadTimeout int  `mapstructure:"download_timeout" json:"download_timeout"`
	RequireLogin    bool `mapstructure:"require_login" json:"require_login"`
	// S3DirectDownload s3 直下：存储后端为 s3 且开启时，文件下载 302 到短时效
	// 预签名 GET URL（下载流量不经过服务器）。env: PB_DOWNLOAD_S3_DIRECT
	S3DirectDownload bool `mapstructure:"s3_direct_download" json:"s3_direct_download"`
	// PresignPolicy 直传策略：everyone=所有人可直传 / authenticated=仅登录用户 /
	// disabled=完全关闭直传（全部走服务器中转）。空串=未设置，按 everyone 处理。
	// 旧开关 presign_anonymous_enabled 已退役（2026-10-26）：存量库该键的语义
	// （false⇒仅登录用户）已并入本字段，迁移由启动期数据迁移完成。
	PresignPolicy string `mapstructure:"presign_policy" json:"presign_policy"`
	// PresignExpireSeconds 直传签名时效（秒）。0=默认 600；钳位 60..3600。
	PresignExpireSeconds int `mapstructure:"presign_expire_seconds" json:"presign_expire_seconds"`
	// PresignThresholdMB 前端直传阈值（MB）。0=默认 100；经 /api/config 下发。
	PresignThresholdMB int `mapstructure:"presign_threshold_mb" json:"presign_threshold_mb"`
	// CodeCaseInsensitive 分享码/自定义口令查询是否不区分大小写（6 位取件码字符表
	// 全大写、天然不敏感，不受此开关影响）。指针三态：nil=未设置（默认不区分，
	// 口播/手抄场景更友好）。开启时自定义口令创建同步按折叠查重，失败锁定键同步
	// 归一。经 /api/config 以下发 codeCaseInsensitive 供前端提示联动。
	CodeCaseInsensitive *bool `mapstructure:"code_case_insensitive" json:"code_case_insensitive"`
}

// CodeFoldEnabled 分享码查询是否折叠大小写（nil 视为开启，兼容存量配置）。
func (c DownloadConfig) CodeFoldEnabled() bool {
	return c.CodeCaseInsensitive == nil || *c.CodeCaseInsensitive
}

// CodeFoldEnabledOrDefault 全局配置未初始化（单测/极早期启动）时按默认开。
func CodeFoldEnabledOrDefault() bool {
	cfg := globalConfig.Load()
	if cfg == nil {
		return true
	}
	return cfg.Download.CodeFoldEnabled()
}

// 直传策略取值（download.presign_policy）
const (
	PresignPolicyEveryone      = "everyone"      // 所有人可直传
	PresignPolicyAuthenticated = "authenticated" // 仅登录用户可直传
	PresignPolicyDisabled      = "disabled"      // 完全关闭直传
)

// PresignPolicyOrDefault 解析生效的直传策略：
// 显式 presign_policy 优先；空串=未设置 ⇒ everyone。
// 旧开关 presign_anonymous_enabled 已退役（2026-10-26）——存量库该键的语义
// （false⇒authenticated）由启动期数据迁移写入 presign_policy，本处不再回退读取。
func (c DownloadConfig) PresignPolicyOrDefault() string {
	switch c.PresignPolicy {
	case PresignPolicyEveryone, PresignPolicyAuthenticated, PresignPolicyDisabled:
		return c.PresignPolicy
	case "":
		return PresignPolicyEveryone
	default:
		return PresignPolicyEveryone // 未知值按开放处理（前端仍有 10015 兜底）
	}
}

// PresignTTLOrDefault 直传签名时效（钳位 60..3600，0=默认 600）。
func (c DownloadConfig) PresignTTLOrDefault() time.Duration {
	if c.PresignExpireSeconds <= 0 {
		return 600 * time.Second
	}
	switch {
	case c.PresignExpireSeconds < 60:
		return 60 * time.Second
	case c.PresignExpireSeconds > 3600:
		return time.Hour
	}
	return time.Duration(c.PresignExpireSeconds) * time.Second
}

// PresignThresholdMBOrDefault 前端直传阈值（0=默认 100MB）。
func (c DownloadConfig) PresignThresholdMBOrDefault() int {
	if c.PresignThresholdMB <= 0 {
		return 100
	}
	return c.PresignThresholdMB
}

// StorageConfig 存储配置
// StorageConfig 存储配置。
// Type 支持：local / s3(含 MinIO) / oss(阿里云) / cos(腾讯云) / bos(百度云) /
// ks3(金山云) / obs(华为云) / webdav。云厂商均走 S3 兼容协议（endpoint 可由
// region 自动推导），各自配置段字段同 CloudStorageConfig。
type StorageConfig struct {
	Type        string `mapstructure:"type" json:"type"` // local, s3, oss, cos, bos, ks3, obs, webdav
	StoragePath string `mapstructure:"storage_path" json:"storage_path"`
	// Quota 站点级全局存储配额（字节，0=不限）。统计口径=存活 file_codes 尺寸合计；
	// 全通道统一闸口（直传/分片完成/预签名完成/本地导入/多文件）。env: PB_STORAGE_QUOTA
	Quota     int64               `mapstructure:"quota" json:"quota"`
	S3        *S3Config           `mapstructure:"s3" json:"s3"`
	WebDAV    *WebDAVConfig       `mapstructure:"webdav" json:"webdav"`
	FTP       *FTPConfig          `mapstructure:"ftp" json:"ftp"`
	SFTP      *SFTPConfig         `mapstructure:"sftp" json:"sftp"`
	AzureBlob *AzureBlobConfig    `mapstructure:"azureblob" json:"azureblob"`
	HDFS      *HDFSConfig         `mapstructure:"hdfs" json:"hdfs"`
	OneDrive  *OneDriveConfig     `mapstructure:"onedrive" json:"onedrive"`
	OSS       *CloudStorageConfig `mapstructure:"oss" json:"oss"`
	COS       *CloudStorageConfig `mapstructure:"cos" json:"cos"`
	BOS       *CloudStorageConfig `mapstructure:"bos" json:"bos"`
	KS3       *CloudStorageConfig `mapstructure:"ks3" json:"ks3"`
	OBS       *CloudStorageConfig `mapstructure:"obs" json:"obs"`
}

// CloudStorageConfig 云厂商对象存储通用配置（S3 兼容协议）。
// Endpoint 留空时按厂商 + Region 自动推导。
type CloudStorageConfig struct {
	Region    string `mapstructure:"region" json:"region"` // 厂商地域，如 ap-guangzhou / cn-hangzhou
	Bucket    string `mapstructure:"bucket" json:"bucket"`
	AccessKey string `mapstructure:"access_key" json:"access_key"`
	SecretKey string `mapstructure:"secret_key" json:"secret_key"`
	Endpoint  string `mapstructure:"endpoint" json:"endpoint"`     // 空 = 按厂商+Region 推导
	UseSSL    *bool  `mapstructure:"use_ssl" json:"use_ssl"`       // 缺省 true
	PathStyle *bool  `mapstructure:"path_style" json:"path_style"` // 缺省 false（各厂商均为 virtual-host 风格）
}

// S3Config S3 兼容对象存储配置（AWS S3 / 阿里云 OSS / 腾讯云 COS 等）
type S3Config struct {
	Endpoint  string `mapstructure:"endpoint" json:"endpoint"`
	Region    string `mapstructure:"region" json:"region"`
	Bucket    string `mapstructure:"bucket" json:"bucket"`
	AccessKey string `mapstructure:"access_key" json:"access_key"`
	SecretKey string `mapstructure:"secret_key" json:"secret_key"`
	UseSSL    bool   `mapstructure:"use_ssl" json:"use_ssl"`
	PathStyle bool   `mapstructure:"path_style" json:"path_style"`
}

// WebDAVConfig WebDAV 存储配置
type WebDAVConfig struct {
	Endpoint string `mapstructure:"endpoint" json:"endpoint"`
	Username string `mapstructure:"username" json:"username"`
	Password string `mapstructure:"password" json:"password"`
}

// FTPConfig FTP/FTPS 存储配置
type FTPConfig struct {
	Host     string `mapstructure:"host" json:"host"` // host[:port]（port 缺省 21）
	Username string `mapstructure:"username" json:"username"`
	Password string `mapstructure:"password" json:"password"`
	// TLS "true" = 显式 FTPS（AUTH TLS）
	TLS string `mapstructure:"tls" json:"tls"`
	// Root 远端子目录（所有对象挂其下；目录需已存在或可创建）
	Root string `mapstructure:"root" json:"root"`
}

// SFTPConfig SFTP 存储配置（password 与 private_key 二选一，前者优先）
type SFTPConfig struct {
	Host     string `mapstructure:"host" json:"host"` // host[:port]（port 缺省 22）
	Username string `mapstructure:"username" json:"username"`
	Password string `mapstructure:"password" json:"password"`
	// PrivateKey PEM 私钥内容（多行，env 注入友好）
	PrivateKey string `mapstructure:"private_key" json:"private_key"`
	// HostKey 可选；known_hosts 行或 base64 主机公钥（配置后严格校验，缺省宽松）
	HostKey string `mapstructure:"host_key" json:"host_key"`
	Root    string `mapstructure:"root" json:"root"`
}

// AzureBlobConfig Azure Blob 存储配置（共享密钥或 SAS 二选一）
type AzureBlobConfig struct {
	Account   string `mapstructure:"account" json:"account"`
	Container string `mapstructure:"container" json:"container"`
	// Key 共享密钥（account key）；SAS 非空时忽略
	Key string `mapstructure:"key" json:"key"`
	// SAS 容器级或账户级 SAS token（以 ? 开头的查询串）
	SAS      string `mapstructure:"sas" json:"sas"`
	Endpoint string `mapstructure:"endpoint" json:"endpoint"` // 可选；Azurite/主权云（缺省 https://<account>.blob.core.windows.net）
	Root     string `mapstructure:"root" json:"root"`
}

// HDFSConfig HDFS 存储配置（WebHDFS REST，NameNode http(s) 端口）
type HDFSConfig struct {
	// Endpoint WebHDFS 根地址，如 http://namenode:9870
	Endpoint string `mapstructure:"endpoint" json:"endpoint"`
	// User HDFS 代理用户（doAs）
	User string `mapstructure:"user" json:"user"`
	// Kerberos/Token 认证为扩展点（当前支持无认证/简单代理用户）
	Root string `mapstructure:"root" json:"root"`
}

// OneDriveConfig OneDrive（Microsoft Graph）存储配置。
// 授权：在 Azure AD 注册应用（Files.ReadWrite.All + offline_access），
// 走一次授权码流程拿 refresh_token 后配置至此，运行时自动刷新 access token。
type OneDriveConfig struct {
	ClientID     string `mapstructure:"client_id" json:"client_id"`
	ClientSecret string `mapstructure:"client_secret" json:"client_secret"`
	// RefreshToken 一次性授权获得，长期有效（轮换由服务端自动处理）
	RefreshToken string `mapstructure:"refresh_token" json:"refresh_token"`
	// Tenant common（个人+组织）或具体租户/消费者域
	Tenant string `mapstructure:"tenant" json:"tenant"` // 缺省 common
	// DriveID 缺省 me/drive；站点/共享库填对应 drive id
	DriveID string `mapstructure:"drive_id" json:"drive_id"`
	Root    string `mapstructure:"root" json:"root"`
}

// UIConfig 前端 UI 相关配置。
// Background/AccentColor 构成安全版主题：仅接受 http(s) 图片 URL 与 #RGB/#RRGGBB
// 颜色值（serve 时校验，非白名单内容整体忽略）——不接受自由 CSS，杜绝旧单体
// background 配置的 CSS 注入面（上游 2.6.0 同类修复）。showAdminAddr：公开页脚
// 是否展示管理后台入口（缺省展示，用 ShowAdminAddrEnabled 取值；/admin 始终可
// 直达，此键仅控制入口可见性）。
type UIConfig struct {
	RobotsText string `mapstructure:"robots_text" json:"robots_text"`
	// ShowAdminAddr 缺省（nil/未配置）= 展示页脚管理入口；显式 false 才隐藏。
	ShowAdminAddr *bool  `mapstructure:"show_admin_addr" json:"show_admin_addr"`
	Background    string `mapstructure:"background" json:"background"`
	AccentColor   string `mapstructure:"accent_color" json:"accent_color"`
	// ExposeOpenAPI 是否公开 /openapi.json（默认 true，前端 /api-docs 页依赖）；
	// 生产部署可置 false 收缩端点清单侦察面（env: PB_UI_EXPOSE_OPENAPI）。
	ExposeOpenAPI bool `mapstructure:"expose_openapi" json:"expose_openapi"`
	// AdminPath 自定义管理后台入口路径段（hash 路由，缺省 admin；env: PB_UI_ADMIN_PATH）。
	// 仅入口混淆（SPA 须知路径才能路由，会随公开配置下发）——防误触与批量扫描，
	// 不构成安全边界，安全靠登录鉴权。非法字符/超长自动回退 admin。
	AdminPath string `mapstructure:"admin_path" json:"admin_path"`
}

// ShowAdminAddrEnabled 页脚管理入口是否展示（nil 视为展示）。
func (c *UIConfig) ShowAdminAddrEnabled() bool {
	return c.ShowAdminAddr == nil || *c.ShowAdminAddr
}

// AdminPathOrDefault 管理后台入口路径段（缺省 "admin"）。
// 白名单消毒：仅字母/数字/-/_，长度 2~32；其余形态一律回退默认值。
func (c *UIConfig) AdminPathOrDefault() string {
	p := strings.TrimSpace(c.AdminPath)
	p = strings.Trim(p, "/")
	if len(p) < 2 || len(p) > 32 {
		return "admin"
	}
	for i := 0; i < len(p); i++ {
		ch := p[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '-', ch == '_':
		default:
			return "admin"
		}
	}
	return p
}

// ObservabilityConfig 可观测性配置（metrics；分布式追踪见路线图，未实现前不暴露配置）
type ObservabilityConfig struct {
	Metrics MetricsConfig `mapstructure:"metrics"`
}

// MetricsConfig Prometheus 指标配置
type MetricsConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	Path    string `mapstructure:"path"` // 指标暴露路径，默认 /metrics
}

// IsProduction 是否生产模式（综合 server.mode 与 app.production 判断）
func (c *AppConfiguration) IsProduction() bool {
	return c.App.Production || c.Server.Mode == "release"
}

// SecurityConfig 安全相关配置（CORS / 可信代理 / 下载令牌 / 防爆破锁定 / SSRF / API Key / OIDC）
type SecurityConfig struct {
	CORS           CORSConfig          `mapstructure:"cors"`
	TrustedProxies []string            `mapstructure:"trusted_proxies"` // 可信代理 CIDR 列表，如 ["10.0.0.0/8","173.245.48.0/20"]
	DownloadToken  DownloadTokenConfig `mapstructure:"download_token"`
	Lockout        LockoutConfig       `mapstructure:"lockout"`
	SSRF           SSRFConfig          `mapstructure:"ssrf"`
	APIToken       APITokenConfig      `mapstructure:"api_token"`
	OIDC           OIDCConfig          `mapstructure:"oidc"`
}

// OIDCConfig OIDC 单点登录配置（P2；默认关闭。启用需 issuer/client_id/client_secret，
// 回调地址 <base_url>/api/v1/user/oidc/callback）
type OIDCConfig struct {
	Enabled          bool   `mapstructure:"enabled" json:"enabled"`                     // env: PB_OIDC_ENABLED
	Issuer           string `mapstructure:"issuer" json:"issuer"`                       // env: PB_OIDC_ISSUER
	ClientID         string `mapstructure:"client_id" json:"client_id"`                 // env: PB_OIDC_CLIENT_ID
	ClientSecret     string `mapstructure:"client_secret" json:"client_secret"`         // env: PB_OIDC_CLIENT_SECRET
	Scopes           string `mapstructure:"scopes" json:"scopes"`                       // 默认 "openid profile email"；env: PB_OIDC_SCOPES
	FrontendCallback string `mapstructure:"frontend_callback" json:"frontend_callback"` // 默认 /#/oidc/callback
}

// APITokenConfig 用户级 API Key（个人访问令牌，pb_sk_）。
// Enabled 为认证总开关（env PB_API_TOKEN_ENABLED）：false 时携带 Key 的请求一律 401。
// PerKeyQPS 为单 Key 独立限流（令牌桶，进程内）：0 = 不限（默认 20，burst 默认 2×QPS）。
type APITokenConfig struct {
	Enabled     bool `mapstructure:"enabled" json:"enabled"`             // 默认 true
	PerKeyQPS   int  `mapstructure:"per_key_qps" json:"per_key_qps"`     // 默认 20；显式 0 = 不限
	PerKeyBurst int  `mapstructure:"per_key_burst" json:"per_key_burst"` // 默认 0 = 2×QPS
}

// DownloadTokenConfig 取件下载令牌（时间窗 HMAC）。
// 启用后 /share/download 必须携带取件查询接口下发的 token（防取件码扫描直下）。
type DownloadTokenConfig struct {
	Enabled         bool `mapstructure:"enabled"`          // 默认 true
	ValiditySeconds int  `mapstructure:"validity_seconds"` // 令牌有效期，默认 1000s（对齐上游）
}

// LockoutConfig 登录/取件失败计数锁定（防爆破，非 QPS 语义）。
type LockoutConfig struct {
	Enabled       bool `mapstructure:"enabled"`        // 默认 true
	MaxAttempts   int  `mapstructure:"max_attempts"`   // 窗口内最大失败次数，默认 10
	WindowSeconds int  `mapstructure:"window_seconds"` // 计数窗口，默认 300s
	LockSeconds   int  `mapstructure:"lock_seconds"`   // 触发后锁定时长，默认 600s
}

// SSRFConfig 存储端点 SSRF 防护。
type SSRFConfig struct {
	// AllowPrivateNetworks 是否允许 s3/webdav 端点指向私网地址。
	// 默认 false（阻止内网探测）；局域网 MinIO/WebDAV（飞牛 NAS）部署需显式开启。
	AllowPrivateNetworks bool `mapstructure:"allow_private_networks"`
}

// RateLimitSettings 限流配置（代码内默认值见 middleware.DefaultRateLimitConfig）。
// Enabled 用指针区分"未配置"（默认开）与"显式 false"。
type RateLimitSettings struct {
	Enabled      *bool `mapstructure:"enabled"`
	GlobalQPS    int   `mapstructure:"global_qps"`
	UploadQPS    int   `mapstructure:"upload_qps"`
	DownloadQPS  int   `mapstructure:"download_qps"`
	LoginQPS     int   `mapstructure:"login_qps"`
	Burst        int   `mapstructure:"burst"`
	BlockSeconds int   `mapstructure:"block_seconds"`
	UseRedis     bool  `mapstructure:"use_redis"` // true=Redis 分布式计数（多实例共享），Redis 不可用自动回退内存
}

// CORSConfig 跨域配置。
//
// 安全说明：当 AllowCredentials=true 时，AllowOrigins 不可为 "*"（浏览器规范），
// 必须显式列出可信来源。若 AllowOrigins 为空且 AllowCredentials=true，
// 中间件会退化为反射 Origin（仅适合开发环境）。
type CORSConfig struct {
	AllowOrigins     []string `mapstructure:"allow_origins"`     // 可信来源列表，env: PB_CORS_ALLOW_ORIGINS（逗号分隔）
	AllowCredentials bool     `mapstructure:"allow_credentials"` // 是否允许携带凭证
	// EnableHSTS 三态（2026-10-08 加固）：nil（未配置）= 生产模式默认开/开发默认关；
	// 显式 true/false 恒以配置为准（自签证书等特殊拓扑的逃生开关）。
	// env: PB_ENABLE_HSTS
	EnableHSTS *bool `mapstructure:"enable_hsts"`
}
