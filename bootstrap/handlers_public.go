package bootstrap

// 职责：公开/探针端点 handler——publicConfigHandler、metricsHandler、readinessHandler、
// initPreviewService 及其白名单校验辅助（safeImageURL/safeHexColor 等）。

import (
	"bytes"
	"context"
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
		"uploadSize":  config.Upload.UploadSize,
		"enableChunk": config.Upload.EnableChunk,
		"openUpload":  config.Upload.OpenUpload,
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
		"presignPolicy":      effectiveDownload.PresignPolicyOrDefault(),
		"presignEnabled":     effectiveDownload.PresignPolicyOrDefault() != conf.PresignPolicyDisabled,
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
	})
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
func effectiveExpireStyles() []string {
	all := []string{"minute", "hour", "day", "week", "month", "year", "forever"}
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
			"code":    500,
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

func initPreviewService() error {
	previewConfig := &previewPkg.Config{
		EnablePreview:    true,
		ThumbnailWidth:   300,
		ThumbnailHeight:  200,
		MaxFileSize:      50 * 1024 * 1024,
		PreviewCachePath: "./data/previews",
		FFmpegPath:       "ffmpeg",
	}

	return previewPkg.InitService(previewConfig)
}
