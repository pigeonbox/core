package bootstrap

// 职责：公开/探针端点 handler——publicConfigHandler、metricsHandler、readinessHandler、
// initPreviewService 及其白名单校验辅助（safeImageURL/safeHexColor 等）。

import (
	"bytes"
	"context"
	"github.com/pigeonbox/contracts/errcode"
	"path/filepath"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
	"go.uber.org/zap"

	configApp "github.com/pigeonbox/core/app/config"
	setupApp "github.com/pigeonbox/core/app/setup"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/logger"
	"github.com/pigeonbox/core/pkg/resp"
	"github.com/pigeonbox/core/pkg/utils"
	previewPkg "github.com/pigeonbox/core/preview"
)

// publicConfigHandler 返回公开配置（前端 configStore 启动时拉取）。
// 返回结构与前端 PublicConfig 接口对齐：
// name / description / uploadSize / enableChunk / openUpload / expireStyle / initialized。
// initialized 为真实初始化状态（按管理员用户数判断）；查询出错时保守返回 true，
// 避免数据库抖动把正常实例的前端误导入 Setup 流程。
func publicConfigHandler(ctx context.Context, c *app.RequestContext) {
	initialized := true
	if ok, err := publicSetupSvc.IsSystemInitialized(ctx); err == nil {
		initialized = ok
	}
	// 站名/描述走生效值：管理后台"站点配置"持久化段优先，
	// 无记录回退 yaml app 段（修复首页展示 yaml 旧品牌名的分裂）
	name, description := config.App.Name, config.App.Description
	effectiveDownload := config.Download
	// 站点公网地址（2026-10-22）：管理后台 base.public_url 优先，缺省回退
	// env/yaml 的 server.base_url；都没有 = 未配置（前端回退浏览器当前 origin）。
	// 与 share/presign 域的 baseURL provider 同源（bootstrap/wiring.go 的
	// publicBaseURL）——前端拿到的地址与服务端拼链接用的地址必须是同一个，
	// 否则用户看到的链接与自己发的链接对不上。
	// 口径收敛到 effectivePublicURL（bootstrap/capabilities.go）——三处取同一值。
	publicURL := effectivePublicURL(ctx)
	if cfg, err := configApp.Default().GetConfig(ctx); err == nil && cfg != nil {
		if cfg.Base.Name != "" {
			name = cfg.Base.Name
		}
		if cfg.Base.Description != "" {
			description = cfg.Base.Description
		}
		if cfg.Download != nil {
			effectiveDownload = *cfg.Download
		}
	}
	resp.Success(c, map[string]interface{}{
		"name":        name,
		"description": description,
		// 站点公网地址（2026-10-22）：供前端拼分享链接与交付文本。
		// 空串 = 未配置 → 前端回退 location.origin。
		"publicUrl": publicURL,
		// 联邦是否启用（2026-10-22）：前端据此决定取件台要不要显式给出
		// 「跨站取件」提示。生效值 = overlay 后的全局 conf（多副本模式下
		// 联邦被强制降级关闭，见 bootstrap/deployment.go 的降级分支）。
		"federationEnabled": config.Federation.Enabled,
		"uploadSize":        config.Upload.UploadSize,
		// 站点级单文件上限（upload.max_file_size，字节，0=不限）：前端上传提示
		// 动态化数据源——2026-10-19 事故中提示硬编码「5GB」与真实生效限制脱节。
		// 登录用户的用户级上限随鉴权而定，不上公共端点，超限时由服务端
		// typed error 文案给出准确值（CodeTooLarge→413）。
		"maxFileSize": config.Upload.MaxFileSize,
		// 站点默认「单次发送**总量**上限」（user.user_upload_size，字节，0=不限）：
		// 一次发送（多文件合计）的体积上限，**只约束登录用户**（匿名走访客日配额
		// upload.anonymous_daily_bytes）。⚠️ 与上面的 maxFileSize（**单文件**上限）
		// 是两个不同的闸——2026-10-30 事故：前端只显示单文件上限（4GB），登录用户
		// 却被总量闸（默认 50MB）卡住，报「上传总大小超过限制（上限 52428800 字节）」。
		// ⇒ 两个旋钮必须一起可见，否则 UI 与真实限制必然对不上。
		// 取值 = 管理后台持久化的 user 段（无记录回退 yaml），与 GetUploadSizeCap 的
		// **系统默认层**同源；**不含** per-user 覆盖（登录用户私有值，不上公开面——
		// 服务端 413 才是权威，本值只用于前端**提示**）。
		"maxBatchSize": effectiveUploadBatchCap(ctx),
		"enableChunk":  config.Upload.EnableChunk,
		"openUpload":   config.Upload.OpenUpload,
		// 注册开关走生效值：管理后台"用户配置"持久化段优先，
		// 无记录回退 yaml（与 /user/register 判定同源）
		"registerEnabled": configApp.EffectiveUserSettings(ctx).AllowUserRegistration,
		// 前端 expireStyle 下拉选项：管理台"允许的过期样式"裁剪优先（对标上游
		// expire_style 白名单驱动前端可选集），未配置=全量
		"expireStyle": effectiveExpireStyles(),
		// 文本分享大小上限（字节；前端预检+超限引导"改用文件分享"）。
		// 与 utils.GetTextShareMaxBytes 同语义：未配置(<=0)用默认 222KB
		"textMaxBytes": textShareMaxBytesOrDefault(),
		"initialized":  initialized,
		// OIDC 登录按钮开关（P2 SSO；security.oidc.enabled）
		"oidcEnabled": conf.GetGlobalConfig().Security.OIDC.Enabled,
		// 管理入口可见性（ui.show_admin_addr，缺省=展示；/admin 路由始终可达，仅控制页脚入口展示）
		"showAdminAddr": config.UI.ShowAdminAddrEnabled(),
		// 管理入口路径段（ui.admin_path/PB_UI_ADMIN_PATH，缺省 admin）：前端按此
		// 注册 hash 路由——自定义后 /admin 不再可达（入口混淆，非安全边界）
		"adminPath": config.UI.AdminPathOrDefault(),
		// 直传设置下发（前端通道决策）：匿名 presign 直传开关 + 直传阈值(MB)。
		// 管理后台持久化段优先，无记录回退全局 conf（缺省=开启/100MB）
		"presignPolicy": effectiveDownload.PresignPolicyOrDefault(),
		// ⚠️ 不再下发 presignEnabled（2026-10-25 删除旧版本兼容）：它是 presignPolicy
		// 的**派生布尔**，只为还不认识 presignPolicy 的旧前端存在 ⇒ 同一事实两个字段
		// 迟早会漂移（前端拿旧的派生值、后端按新策略放行）。前端已改为只读
		// presignPolicy（缺字段即 fail-closed）。
		"presignThresholdMb": effectiveDownload.PresignThresholdMBOrDefault(),
		// 分享码/口令查询大小写折叠（download.code_case_insensitive，缺省=开）：
		// 前端首页提示联动（"大小写不限"/"区分大小写"）
		"codeCaseInsensitive": effectiveDownload.CodeFoldEnabled(),
		// API 文档开关（ui.expose_openapi）：false 时后端 /openapi.json 404，
		// 前端据此隐藏 API 文档入口并将 /api-docs 页降级为未开启提示
		"apiDocsEnabled": config.UI.ExposeOpenAPI,
		// 安全版主题（serve 时白名单校验：非 http(s) URL / 非 #hex 颜色整体忽略）
		"background":  safeImageURL(config.UI.Background),
		"accentColor": safeHexColor(config.UI.AccentColor),
		// 能力自描述（2026-10-30）：「本面支持什么」的一份真相源。
		// 公开面只放匿名可见也无害的四项（部署模式 / 站点地址是否配置 /
		// 首启是否要令牌 / 跨站投递是否可用）；存储后端、Redis 模式等管理面
		// 事实走 GET /admin/diagnostics（见 bootstrap/diagnostics.go）。
		// ⚠️ 客户端事实（WebCrypto 是否可用、Beam 是否挂载）**不在这里**——
		// 服务端无从得知，由前端探测（utils/e2e.ts、components/share/deliveryPath.ts）。
		"capabilities": publicCapabilitySnapshot(ctx),
	})
}

// effectiveUploadBatchCap 生效的站点默认「单次发送**总量**上限」（字节，0=不限）。
//
// 与「单文件上限」（upload.max_file_size ⇒ /api/config 的 maxFileSize）是两个不同的闸：
//
//	· maxFileSize：单个文件的体积上限
//	· maxBatchSize：一次发送（多文件合计）的体积上限，**只约束登录用户**
//	  （匿名走访客日配额 upload.anonymous_daily_bytes）
//
// 取值 = 管理后台持久化的 user 段（无记录回退 yaml），与 user 域
// GetUploadSizeCap 的**系统默认层**同源；**不含** per-user 覆盖
// （users.max_upload_size 是登录用户私有值，不上公开面——服务端 413 才是权威，
// 本值只用于前端**提示**）。
func effectiveUploadBatchCap(ctx context.Context) int64 {
	return configApp.Default().EffectiveUploadBatchCap(ctx)
}

// safeImageURL 背景 URL 白名单校验：仅 http(s)，其余返回空（防 CSS/JS 注入）
func safeImageURL(raw string) string {
	u := strings.TrimSpace(raw)
	if (strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")) && !strings.ContainsAny(u, "\"'<>{}") {
		return u
	}
	return ""
}

// textShareMaxBytesOrDefault 文本分享上限（字节），<=0 回退 222KB（对齐上游默认）
func textShareMaxBytesOrDefault() int64 {
	if v := config.Upload.TextMaxBytes; v > 0 {
		return v
	}
	return 222 * 1024
}

// effectiveExpireStyles 前端可选过期样式：管理台 AllowedExpireStyles 裁剪优先，
// 空（未配置）回退全量——与 utils.CheckExpireStyleAllowed 的校验口径同源。
//
// 2026-10-22：此处的「全量」原是硬编码的 7 个（minute..forever），**漏了
// "count"（按取件次数过期）** ⇒ 即便管理员在首启向导/管理台勾选了 count，
// /api/config 下发给前端的可选项里也没有它 ⇒ **用户在界面上根本选不到**
// 「对方只能取 N 次」这道护栏（而 share 域的 utils 一直认 count）。
// 改为复用 pkg/utils 的合法集合（唯一真相源），三处白名单不再各写一份。
func effectiveExpireStyles() []string {
	all := utils.AllExpireStyles()
	allowed := config.Upload.AllowedExpireStyles
	if len(allowed) == 0 {
		return all
	}
	out := make([]string, 0, len(allowed))
	for _, a := range allowed {
		for _, style := range all {
			if a == style {
				out = append(out, style)
				break
			}
		}
	}
	return out
}

// safeHexColor 主题色白名单校验：#RGB/#RRGGBB/#RRGGBBAA
func safeHexColor(raw string) string {
	u := strings.TrimSpace(raw)
	if len(u) >= 4 && len(u) <= 9 && strings.HasPrefix(u, "#") {
		ok := true
		for _, c := range u[1:] {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				ok = false
				break
			}
		}
		if ok {
			return u
		}
	}
	return ""
}

// publicSetupSvc /api/config 查询初始化状态用（initThriftIDLServices 装配赋值；
// 包级 var 在 InitDatabase 之前求值的自建构造已随 dao 去全局化移除）
var publicSetupSvc *setupApp.Service

// metricsHandler 暴露 Prometheus 指标（/metrics）。
// Hertz 与标准 net/http 接口不同，不能直接用 promhttp.Handler()，
// 这里手动 gather 指标并用 expfmt 文本格式输出到 buffer 再写入响应。
func metricsHandler(ctx context.Context, c *app.RequestContext) {
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		c.JSON(consts.StatusInternalServerError, map[string]interface{}{
			"code":    errcode.CodeInternal,
			"message": "failed to gather metrics: " + err.Error(),
		})
		return
	}
	var buf bytes.Buffer
	enc := expfmt.NewEncoder(&buf, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			logger.Error("failed to encode metric", zap.String("name", mf.GetName()), zap.Error(err))
		}
	}
	c.SetStatusCode(consts.StatusOK)
	c.Response.Header.SetContentType(string(expfmt.NewFormat(expfmt.TypeTextPlain)))
	_, _ = c.Write(buf.Bytes())
}

// readinessHandler 深度就绪检查：校验 DB 连通性，失败返回 503。
// 供 K8s readinessProbe 使用——依赖未就绪时不接流量。
func readinessHandler(ctx context.Context, c *app.RequestContext) {
	checks := map[string]bool{}
	allOK := true

	// DB ping
	if database != nil {
		if sqlDB, err := database.DB(); err == nil {
			if err := sqlDB.Ping(); err == nil {
				checks["database"] = true
			} else {
				checks["database"] = false
				allOK = false
			}
		} else {
			checks["database"] = false
			allOK = false
		}
	} else {
		checks["database"] = false
		allOK = false
	}

	status := "ready"
	httpStatus := consts.StatusOK
	if !allOK {
		status = "not ready"
		httpStatus = consts.StatusServiceUnavailable
	}
	c.JSON(httpStatus, map[string]interface{}{
		"status": status,
		"checks": checks,
	})
}

// initPreviewService 初始化预览服务。缓存目录必须落在**已解析的数据目录**下，
// 不可用相对路径 "./data/previews"——相对路径按进程 CWD 解析，而 NAS 包
// （fnOS 等）、容器与 systemd 部署的 CWD 都不指向数据目录 ⇒ 建目录 EACCES ⇒
// 预览服务初始化失败（真机实证 2026-10-28：fnOS 4.0.7 每次启动都报
// "failed to create preview cache directory: mkdir ./data/previews: permission denied"，
// 该错误自 3.3.x 起长期存在，非新版本引入）。
//
// 连带风险（本函数 init 失败即触发，故必须连同修）：
//
//	① preview.GetService() 返回 nil ⇒ MaxFileSize() 走 nil 接收者分支返回 0
//	   （=不限）⇒ 2026-10-08 加固的「预览大小上限」被**静默关掉**；
//	② app/preview 的 nil 检查排在「文件全量暂存到临时盘」之后 ⇒ 任意大文件
//	   仍会先落满临时盘再报错 ⇒ 磁盘耗尽 DoS 面回归。
//
// 故：缓存目录 = 已解析数据目录/previews；数据目录为空时回退 "./data"
// （与 app/storage 的兜底口径一致）。
func initPreviewService(dataPath string) error {
	if dataPath == "" {
		dataPath = "./data"
	}
	previewConfig := &previewPkg.Config{
		EnablePreview:    true,
		ThumbnailWidth:   300,
		ThumbnailHeight:  200,
		MaxFileSize:      50 * 1024 * 1024,
		PreviewCachePath: filepath.Join(dataPath, "previews"),
		FFmpegPath:       "ffmpeg",
	}

	return previewPkg.InitService(previewConfig)
}
