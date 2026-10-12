package bootstrap

// 职责：配置初始化——InitConfig、代码内默认值、env 绑定表与敏感密钥 fail-fast 校验。

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/pigeonbox/core/conf"
	"github.com/spf13/viper"
)

// InitConfig 初始化配置。
//
// 配置来源优先级（高 → 低）：
//  1. 环境变量（PB_ 前缀完整名 / 文档化的短名，见 bindEnvironment）
//  2. 配置文件（yaml，路径由 configPath 或 CONFIG_PATH 决定）
//  3. 代码内默认值（SetDefault）
//
// 这样容器化部署（K8s/Docker）可通过 env 注入敏感配置（jwt_secret、db 密码等），
// 而无需修改镜像内的配置文件，符合 12-factor。
func InitConfig(configPath string) (*Config, error) {
	// 解析最终配置文件路径：参数 > CONFIG_PATH env > 默认
	if configPath == "" {
		configPath = os.Getenv("CONFIG_PATH")
	}
	if configPath == "" {
		configPath = "configs/config.yaml"
	}

	v := viper.New()
	v.SetConfigFile(configPath)
	v.SetConfigType("yaml")

	// 代码内默认值（仅当文件与 env 均未设置时生效）
	setDefaults(v)

	// 绑定环境变量（优先级最高）
	bindEnvironment(v)

	// 读取配置文件（缺失时降级为仅默认值 + env，便于无文件启动）
	if err := v.ReadInConfig(); err != nil {
		log.Printf("Warning: Failed to read config file %s: %v, using defaults + env", configPath, err)
	} else {
		log.Printf("Loaded config from: %s", v.ConfigFileUsed())
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// 生产环境敏感配置 fail-fast 校验
	if err := validateSecrets(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// setDefaults 设置代码内默认值。
func setDefaults(v *viper.Viper) {
	v.SetDefault("server.host", "0.0.0.0")
	v.SetDefault("server.port", 12345)
	v.SetDefault("server.mode", "debug")
	v.SetDefault("server.base_url", "")
	v.SetDefault("database.driver", "sqlite")
	v.SetDefault("database.db_name", "./data/pigeonbox.db")
	// auto_migrate 默认 true:env-only 形态(无 config.yaml,如 openwrt/fnos 容器)
	// 此键无 yaml/env 来源,若取 Go 零值 false 会跳过 standalone 迁移步骤,
	// 全新安装缺 notifies 表 → /notifies/* 500(2026-10-07 真机事故)
	v.SetDefault("database.auto_migrate", true)
	v.SetDefault("user.allow_user_registration", true)
	v.SetDefault("observability.metrics.enabled", false)
	v.SetDefault("observability.metrics.path", "/metrics")
	// 部署模式默认单机全功能（多副本拆分见 docs/specs/2026-10-06-multi-replica-deployment-modes.md）
	v.SetDefault("deployment.mode", conf.DeployModeStandalone)
	// 安全默认（安全加固项，未配置时全开）：
	v.SetDefault("upload.enable_magic_check", true)
	v.SetDefault("security.download_token.enabled", true)
	v.SetDefault("security.download_token.validity_seconds", 1000)
	v.SetDefault("security.lockout.enabled", true)
	v.SetDefault("security.lockout.max_attempts", 10)
	v.SetDefault("security.lockout.window_seconds", 300)
	v.SetDefault("security.lockout.lock_seconds", 600)
	v.SetDefault("security.api_token.enabled", true)
	v.SetDefault("security.api_token.per_key_qps", 20)
	v.SetDefault("security.api_token.per_key_burst", 40)
	v.SetDefault("rate_limit.enabled", true)
	v.SetDefault("rate_limit.global_qps", 100)
	v.SetDefault("rate_limit.upload_qps", 10)
	v.SetDefault("rate_limit.download_qps", 50)
	v.SetDefault("rate_limit.login_qps", 5)
	v.SetDefault("rate_limit.burst", 20)
	v.SetDefault("rate_limit.block_seconds", 60)
	// MCP server 默认开启（路由挂管理员认证，无暴露风险）
	v.SetDefault("mcp.enabled", true)
	// 访客快速分享默认关闭（管理员专属；放开走 PB_MCP_ALLOW_GUEST=true，
	// 游客仅 share_text/share_file，同网页匿名寄件限流/审核链路）
	v.SetDefault("mcp.allow_guest", false)
	v.SetDefault("mcp.max_file_size", 6<<20) // 单文件上限；默认 6MB（base64 后约 8MB < 默认请求体上限 10MB）
	// /openapi.json 公开开关：默认保持公开（前端 /api-docs 页依赖），
	// 生产部署建议关闭以收缩端点清单侦察面（PB_UI_EXPOSE_OPENAPI=false）
	v.SetDefault("ui.expose_openapi", true)
	// 内容审核默认关闭、命中默认直接拒绝（治理 2026-10-03）
	v.SetDefault("moderation.enabled", false)
	v.SetDefault("moderation.block_action", "reject")
	v.SetDefault("admin.log_retention_days", 90)
	// P2P 联邦（M2）默认关闭；启用须显式配置 registry_url + public_url
	v.SetDefault("federation.enabled", false)
	v.SetDefault("federation.node_key_path", "data/federation.key")
	v.SetDefault("federation.announce_min_entropy_bits", 40)
}

// envBindings 环境变量 → 配置 key 的映射。
// 同时支持两套命名：
//   - 文档化的短扁平名（PORT / DATABASE_HOST 等，便于运维记忆）
//   - PB_ 前缀 + 下划线的完整名（PB_SERVER_PORT，与 mapstructure key 对齐）
var envBindings = map[string][]string{
	// deployment（多副本部署模式）
	"deployment.mode": {"PB_DEPLOY_MODE"},
	// server
	"server.host":          {"PB_SERVER_HOST", "HOST"},
	"server.port":          {"PB_SERVER_PORT", "PORT"},
	"server.mode":          {"PB_SERVER_MODE"},
	"server.base_url":      {"PB_SERVER_BASE_URL", "BASE_URL"},
	"server.read_timeout":  {"PB_SERVER_READ_TIMEOUT"},
	"server.write_timeout": {"PB_SERVER_WRITE_TIMEOUT"},
	// database
	"database.driver":   {"PB_DATABASE_DRIVER", "DATABASE_TYPE", "DB_TYPE"},
	"database.db_name":  {"PB_DATABASE_DB_NAME", "DATABASE_NAME", "DB_NAME"},
	"database.host":     {"PB_DATABASE_HOST", "DATABASE_HOST", "DB_HOST"},
	"database.port":     {"PB_DATABASE_PORT", "DATABASE_PORT", "DB_PORT"},
	"database.user":     {"PB_DATABASE_USER", "DATABASE_USER", "DB_USER"},
	"database.password": {"PB_DATABASE_PASSWORD", "DATABASE_PASS", "DB_PASS"},
	// redis
	"redis.host":     {"PB_REDIS_HOST", "REDIS_HOST"},
	"redis.port":     {"PB_REDIS_PORT", "REDIS_PORT"},
	"redis.password": {"PB_REDIS_PASSWORD", "REDIS_PASSWORD"},
	"redis.db":       {"PB_REDIS_DB", "REDIS_DB"},
	// app
	"app.datapath":   {"PB_DATA_PATH", "DATA_PATH"},
	"app.production": {"PB_PRODUCTION", "PRODUCTION"},
	// user
	"user.jwt_secret":              {"PB_JWT_SECRET", "JWT_SECRET"},
	"user.allow_user_registration": {"PB_USER_ALLOW_REGISTRATION"},
	// upload
	"upload.open_upload": {"PB_OPEN_UPLOAD", "OPEN_UPLOAD"},
	"upload.upload_size": {"PB_UPLOAD_SIZE", "UPLOAD_SIZE"},
	// storage
	"storage.type":         {"PB_STORAGE_TYPE"},
	"storage.storage_path": {"PB_STORAGE_PATH"},
	"storage.quota":        {"PB_STORAGE_QUOTA"},
	// storage.s3(config.example.yaml 注明"生产请用 env 注入",此前映射缺失,现补齐)
	"storage.s3.access_key": {"PB_STORAGE_S3_ACCESS_KEY"},
	"storage.s3.secret_key": {"PB_STORAGE_S3_SECRET_KEY"},
	"storage.s3.endpoint":   {"PB_STORAGE_S3_ENDPOINT"},
	"storage.s3.region":     {"PB_STORAGE_S3_REGION"},
	"storage.s3.bucket":     {"PB_STORAGE_S3_BUCKET"},
	"storage.s3.use_ssl":    {"PB_STORAGE_S3_USE_SSL"},
	"storage.s3.path_style": {"PB_STORAGE_S3_PATH_STYLE"},
	// download
	"download.s3_direct_download": {"PB_DOWNLOAD_S3_DIRECT"},
	// observability
	"observability.metrics.enabled": {"PB_METRICS_ENABLED"},
	"observability.metrics.path":    {"PB_METRICS_PATH"},
	// security
	"security.cors.allow_origins":          {"PB_CORS_ALLOW_ORIGINS"},
	"security.cors.enable_hsts":            {"PB_ENABLE_HSTS"},
	"security.trusted_proxies":             {"PB_TRUSTED_PROXIES"},
	"security.download_token.enabled":      {"PB_DOWNLOAD_TOKEN_ENABLED"},
	"security.lockout.enabled":             {"PB_LOCKOUT_ENABLED"},
	"security.api_token.enabled":           {"PB_API_TOKEN_ENABLED"},
	"security.api_token.per_key_qps":       {"PB_API_TOKEN_PER_KEY_QPS"},
	"security.api_token.per_key_burst":     {"PB_API_TOKEN_PER_KEY_BURST"},
	"security.lockout.max_attempts":        {"PB_LOCKOUT_MAX_ATTEMPTS"},
	"security.ssrf.allow_private_networks": {"PB_SSRF_ALLOW_PRIVATE"},
	// security.oidc（单点登录）
	"security.oidc.enabled":       {"PB_OIDC_ENABLED"},
	"security.oidc.issuer":        {"PB_OIDC_ISSUER"},
	"security.oidc.client_id":     {"PB_OIDC_CLIENT_ID"},
	"security.oidc.client_secret": {"PB_OIDC_CLIENT_SECRET"},
	"security.oidc.scopes":        {"PB_OIDC_SCOPES"},
	// notify
	"notify.webhook_url": {"PB_WEBHOOK_URL", "WEBHOOK_URL"},
	// notify.smtp（邮件通知渠道）
	"notify.smtp.host":     {"PB_SMTP_HOST"},
	"notify.smtp.port":     {"PB_SMTP_PORT"},
	"notify.smtp.username": {"PB_SMTP_USERNAME"},
	"notify.smtp.password": {"PB_SMTP_PASSWORD"},
	"notify.smtp.from":     {"PB_SMTP_FROM"},
	// mcp
	"mcp.enabled":       {"PB_MCP_ENABLED"},
	"mcp.allow_guest":   {"PB_MCP_ALLOW_GUEST"},
	"mcp.max_file_size": {"PB_MCP_MAX_FILE_SIZE"},
	// ui
	"ui.expose_openapi":  {"PB_UI_EXPOSE_OPENAPI"},
	"ui.show_admin_addr": {"PB_UI_SHOW_ADMIN_ADDR"},
	"ui.admin_path":      {"PB_UI_ADMIN_PATH"},
	// moderation（内容审核，治理 2026-10-03）
	"moderation.enabled":       {"PB_MODERATION_ENABLED"},
	"moderation.blocked_words": {"PB_MODERATION_BLOCKED_WORDS"},
	"moderation.block_action":  {"PB_MODERATION_BLOCK_ACTION"},
	// moderation.clamav（文件病毒扫描）
	"moderation.clamav.enabled": {"PB_MODERATION_CLAMAV_ENABLED"},
	"moderation.clamav.addr":    {"PB_MODERATION_CLAMAV_ADDR"},
	// admin 运维
	"admin.log_retention_days": {"PB_ADMIN_LOG_RETENTION_DAYS"},
	// federation（P2P 联邦接入）
	"federation.enabled":                   {"PB_FEDERATION_ENABLED"},
	"federation.registry_url":              {"PB_FEDERATION_REGISTRY_URL"},
	"federation.public_url":                {"PB_FEDERATION_PUBLIC_URL"},
	"federation.node_key_path":             {"PB_FEDERATION_NODE_KEY_PATH"},
	"federation.announce_min_entropy_bits": {"PB_FEDERATION_MIN_ENTROPY"},
	// upload 安全项
	"upload.text_max_bytes":        {"PB_TEXT_MAX_BYTES"},
	"upload.allowed_extensions":    {"PB_UPLOAD_ALLOWED_EXTENSIONS"},
	"upload.blocked_extensions":    {"PB_UPLOAD_BLOCKED_EXTENSIONS"},
	"upload.enable_magic_check":    {"PB_ENABLE_MAGIC_CHECK"},
	"upload.max_save_seconds_cap":  {"PB_UPLOAD_MAX_SAVE_SECONDS_CAP"},
	"upload.anonymous_daily_count": {"PB_UPLOAD_ANON_DAILY_COUNT"},
	"upload.anonymous_daily_bytes": {"PB_UPLOAD_ANON_DAILY_BYTES"},
	// upload.local_import（NAS 本地文件免上传导入）
	"upload.local_import.enabled": {"PB_LOCAL_IMPORT_ENABLED"},
	"upload.local_import.roots":   {"PB_LOCAL_IMPORT_ROOTS"},
	// rate_limit
	"rate_limit.enabled":       {"PB_RATE_LIMIT_ENABLED"},
	"rate_limit.global_qps":    {"PB_RATE_LIMIT_GLOBAL_QPS"},
	"rate_limit.upload_qps":    {"PB_RATE_LIMIT_UPLOAD_QPS"},
	"rate_limit.download_qps":  {"PB_RATE_LIMIT_DOWNLOAD_QPS"},
	"rate_limit.login_qps":     {"PB_RATE_LIMIT_LOGIN_QPS"},
	"rate_limit.block_seconds": {"PB_RATE_LIMIT_BLOCK_SECONDS"},
	"rate_limit.use_redis":     {"PB_RATE_LIMIT_USE_REDIS"},
}

// listValuedKeys 值为列表（[]string）的配置 key：env 只能传字符串，
// 需按逗号拆分后写入（viper Unmarshal 的 WeaklyTypedInput 不拆分逗号，
// 会把 ".jpg,.png" 整串当单个元素——此前 allowed_extensions env 一直有此问题）。
var listValuedKeys = map[string]bool{
	"upload.local_import.roots":    true,
	"upload.allowed_extensions":    true,
	"upload.blocked_extensions":    true,
	"upload.allowed_expire_styles": true,
	"security.trusted_proxies":     true,
	"security.cors.allow_origins":  true,
	"moderation.blocked_words":     true,
}

// splitCSV 逗号分隔字符串 → 去空白去空的切片。
func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// bindEnvironment 把环境变量绑定到 viper 配置 key。
// 列表中靠前的 env 名优先（viper BindEnv 只绑定第一个非空）。
func bindEnvironment(v *viper.Viper) {
	for key, envs := range envBindings {
		// 绑定所有候选 env 名；viper 会取最后一个 BindEnv 的值，
		// 因此我们逐个检查并显式设置，确保优先级正确。
		for _, env := range envs {
			if val, ok := os.LookupEnv(env); ok {
				if listValuedKeys[key] {
					v.Set(key, splitCSV(val))
				} else {
					v.Set(key, val)
				}
				break
			}
		}
	}
}

// insecureDefaultSecrets 已知的不安全默认/占位密钥（全环境禁止使用）。
var insecureDefaultSecrets = map[string]string{
	"PigeonBox2025JWT":                     "user.jwt_secret",
	"pigeonbox-dev-signing-key-change-me":  "presign signing key",
	"PigeonBox2025SecretKey":               "auth default secret",
	"please-change-me":                     "placeholder secret",
	"dev-only-change-me":                   "dev placeholder secret",
	"dev-only-change-me-to-random-32chars": "dev placeholder secret",
	// deploy/k8s/base/secret.yaml 的占位值（2026-10-05 审计 P1：公开仓库里的
	// 已知密钥=任何人可离线伪造任意 JWT）。忘记替换时启动即失败，而非带公开
	// 密钥上线。
	"REPLACE_WITH_STRONG_RANDOM_SECRET": "k8s base manifest placeholder",
}

// validateSecrets 全环境校验敏感配置，避免使用默认/弱密钥启动（fail-fast）。
// 所有环境（含开发）都必须设置强随机的 user.jwt_secret。
func validateSecrets(cfg *Config) error {
	// jwt_secret：空或命中黑名单一律拒绝
	if sec := cfg.User.JWTSecret; sec == "" || insecureDefaultSecrets[sec] != "" {
		return fmt.Errorf("a secure user.jwt_secret is required in ALL environments: current value is empty or a known default; set PB_JWT_SECRET env to a strong random string (>=32 chars)")
	}
	return nil
}
