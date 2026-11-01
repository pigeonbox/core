// 跨站投递（联邦前台化，2026-11-01）的两个公开端点。
//
//	POST /api/v1/federation/delivery-slot  对方柜（B 柜）侧：铸访客投递槽位
//	POST /api/v1/federation/deliver       发起柜（A 柜）侧：访客跨站投递
//
// 形态（设计稿：docs/specs/2026-11-01-federation-frontend-delivery-design.md）：
//
//	访客 ──① 在 A 柜前台填「对方柜 URL」+ 选文件 → POST A 柜 /api/v1/federation/deliver
//	        A 柜 ──② 校验目标 ∈ 管理员登记的「可投递对端」（未命中 ⇒ 拒绝，**不发请求**）
//	        A 柜 ──③ 带共享口令调 B 柜铸槽位 → 拿 token
//	        A 柜 ──④ 把文件**流式**转发到 B 柜 /api/v1/request/:token/upload
//	                ⇒ B 柜落库 ⇒ **B 柜的 share 域铸 6 位取件码**
//	        A 柜 ──⑤ 把 B 的 6 位码回给访客
//	取件人 ──⑥ 在 **B 柜** 用该 6 位码取件
//
// 路由由 contracts IDL 生成（publicapi 域 ⇒ 只在公开副本注册），
// gen/handler/publicapi 薄委托到这里（与既有 IDL 化路由同款）。
//
// 信任模型：共享口令 bearer（口令即权限，同现有联邦信任级；常数时间比对）。
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"strings"
	"sync"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/pigeonbox/contracts/errcode"
	"github.com/pigeonbox/core/app/federation"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/pkg/gate"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/pkg/resp"
	"github.com/pigeonbox/core/pkg/security"
	"github.com/pigeonbox/core/pkg/utils"
)

// maxDeliveryFiles 单次跨站投递的文件数上限（与站内多文件直传同款）。
const maxDeliveryFiles = 100

var (
	deliveryClient    *federation.DeliveryClient
	deliveryClientMu  sync.RWMutex
	defaultClient     *federation.DeliveryClient
	defaultClientOnce sync.Once
)

// SetDeliveryClient 注入跨站投递出站客户端（bootstrap 调用；nil = 用默认构造）。
func SetDeliveryClient(c *federation.DeliveryClient) {
	deliveryClientMu.Lock()
	defer deliveryClientMu.Unlock()
	deliveryClient = c
}

// getDeliveryClient 取当前出站客户端（未注入时用**进程级单例**默认构造——
// 每次请求新建一个 http.Client 会丢掉连接池）。
func getDeliveryClient() *federation.DeliveryClient {
	deliveryClientMu.RLock()
	c := deliveryClient
	deliveryClientMu.RUnlock()
	if c != nil {
		return c
	}
	defaultClientOnce.Do(func() {
		defaultClient = federation.NewDeliveryClient()
	})
	return defaultClient
}

// FederationDeliverySlot 对方柜（B 柜）侧：为对面柜铸一个访客投递槽位。
//
// 授权 = 共享口令 bearer（header）；比对用常数时间（防时序侧信道）。
//
// ⚠️ **只回 token**（投递链接令牌），**不回 6 位取件码**——此时还没有文件，
// 对方柜不可能铸出取件码（回了也是假的）。
func FederationDeliverySlot(ctx context.Context, c *app.RequestContext) {
	svc := getRequestService()
	if svc == nil {
		c.JSON(consts.StatusInternalServerError, map[string]interface{}{
			"code": errcode.CodeInternal, "message": "service 未就绪",
		})
		return
	}

	// 授权：共享口令（任一登记对端匹配即通过——口令即权限，不要求知道调用方是谁；
	// 「B 柜认不认 A」这件事由 B 柜自己登记的对端名单裁决）。
	if !federation.VerifyDeliveryToken(string(c.GetHeader(federation.DeliveryTokenHeader))) {
		c.JSON(consts.StatusForbidden, map[string]interface{}{
			"code": errcode.CodeForbidden, "message": "共享口令无效或未登记",
		})
		return
	}

	fromName, note, title := deliverySlotParams(c)
	fr, err := svc.CreateFederationSlot(ctx, title, fromName, note)
	if err != nil {
		c.JSON(consts.StatusInternalServerError, map[string]interface{}{
			"code": errcode.CodeInternal, "message": "铸造投递槽位失败: " + err.Error(),
		})
		return
	}

	resp.Success(c, map[string]interface{}{
		// token = 寄件码 token（投递链接令牌）⇒ 对面柜用它往本站投递文件。
		"token": fr.Token,
	})
}

// deliverySlotParams 读铸槽位请求的三个可选参数。
//
// 契约形态是表单字段（api.body）⇒ 主路径按表单取；同时兼容 JSON 体
// （便于脚本/自测直连）。
func deliverySlotParams(c *app.RequestContext) (fromName, note, title string) {
	if ct := string(c.GetHeader("Content-Type")); strings.Contains(ct, "application/json") {
		var body struct {
			FromName string `json:"from_name"`
			Note     string `json:"note"`
			Title    string `json:"title"`
		}
		if err := json.Unmarshal(c.Request.Body(), &body); err == nil {
			return body.FromName, body.Note, body.Title
		}
		return "", "", ""
	}
	return c.PostForm("from_name"), c.PostForm("note"), c.PostForm("title")
}

// FederationDeliver 发起柜（A 柜）侧：访客跨站投递。
//
// 出站目标**只来自管理员登记的对端**（访客填的 URL 必须命中它）⇒
// ① 误投面：未登记 ⇒ 当场拒，不发出去；
// ② SSRF 面：从「任意 URL」收窄到「admin 配的 URL」。
func FederationDeliver(ctx context.Context, c *app.RequestContext) {
	form, err := c.MultipartForm()
	if err != nil {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{
			"code": errcode.CodeInvalidParam, "message": "解析 multipart 失败: " + err.Error(),
		})
		return
	}
	files := form.File["files"]
	if len(files) == 0 {
		files = form.File["file"] // 兼容单文件字段名
	}
	if len(files) == 0 {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{
			"code": errcode.CodeInvalidParam, "message": "请选择要投递的文件",
		})
		return
	}
	if len(files) > maxDeliveryFiles {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{
			"code": errcode.CodeInvalidParam, "message": "单次投递文件数超过上限（100 个）",
		})
		return
	}

	targetURL := firstFormValue(form.Value["target_url"])
	if targetURL == "" {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{
			"code": errcode.CodeInvalidParam, "message": "缺少对方柜地址（target_url）",
		})
		return
	}

	// ② 对端白名单：**必须命中**。未命中 ⇒ 明确拒绝，且**一个出站请求都不发**。
	peer, ok := federation.MatchDeliveryPeer(targetURL)
	if !ok {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{
			"code": errcode.CodeInvalidParam, "message": "对方柜不在可投递名单",
		})
		return
	}

	// ③ 出站前的 SSRF 复核：私网对端必须显式 allow_private（按对端粒度）。
	if err := validateDeliveryPeer(peer); err != nil {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{
			"code": errcode.CodeEndpointDenied, "message": err.Error(),
		})
		return
	}

	// ④ 体积闸门（转发路径单独算）：
	//   · 单文件上限 = 站点的 upload.max_file_size；
	//   · 总量 = 匿名日配额（访客经本柜中转 ⇒ 计入本柜配额，不能靠「投到别家」
	//     绕开本柜的匿名配额）。
	var totalDeclared int64
	for _, f := range files {
		totalDeclared += f.Size
		if maxFile := utils.GetMaxFileSize(); maxFile > 0 && f.Size > maxFile {
			c.JSON(consts.StatusRequestEntityTooLarge, map[string]interface{}{
				"code": errcode.CodeTooLarge, "message": "文件 " + f.Filename + " 超过单文件大小上限",
			})
			return
		}
	}
	if err := gate.CheckAnonymousQuota(ctx, middleware.ClientIP(c), totalDeclared); err != nil {
		resp.NewTypedError(c, err)
		return
	}

	// ⑤ 铸槽位（口令 bearer）→ ⑥ 流式转发文件。
	client := getDeliveryClient()
	token, err := client.CreateSlot(ctx, peer, deliveryFromName(), firstFormValue(form.Value["note"]), firstFormValue(form.Value["title"]))
	if err != nil {
		c.JSON(consts.StatusBadGateway, map[string]interface{}{
			"code": errcode.CodeUnavailable, "message": "对方柜铸槽位失败: " + err.Error(),
		})
		return
	}
	result, err := client.ForwardUpload(ctx, peer, token, deliveryFilesOf(files))
	if err != nil {
		c.JSON(consts.StatusBadGateway, map[string]interface{}{
			"code": errcode.CodeUnavailable, "message": "对方柜收件失败: " + err.Error(),
		})
		return
	}

	// 回给访客的是**对方柜铸的**取件码（A 柜只透传，不铸码）⇒ 访客在对方柜取件。
	//
	// ⚠️ 对方柜没回码（未铸造）时**不填该键**（optional 语义）⇒ 前端要能兜住
	// 「没拿到码」（别显示一个空白码框）。
	data := map[string]interface{}{
		"file_count": result.FileCount,
		"target_url": peer.URL,
	}
	if result.PickupCode != "" {
		data["pickup_code"] = result.PickupCode
	}
	resp.Success(c, data)
}

// validateDeliveryPeer 出站前的 SSRF 复核（按对端粒度的私网放行）。
//
// allow_private=true 的对端走「放行私网」变体（只校验 scheme/host）；
// 否则走默认策略（私网/保留地址被拒，除非管理员开了全局开关）。
func validateDeliveryPeer(peer conf.FederationDeliveryPeer) error {
	if peer.AllowPrivate {
		if err := security.ValidateEndpointURLAllowPrivate(peer.URL); err != nil {
			return err
		}
		return nil
	}
	if err := security.ValidateEndpointURL(peer.URL); err != nil {
		// ⚠️ 报错文案要说清「按对端粒度怎么放行」（默认策略的文案是给存储端点写的，
		// 直接透传会让管理员以为要去开全局 SSRF 开关）。
		return fmt.Errorf("对方柜地址校验失败（私网目标需对该对端开启 allow_private）: %w", err)
	}
	return nil
}

// deliveryFilesOf 把 multipart 文件头转成「惰性打开」的待转发文件列表。
//
// ⚠️ 惰性打开：转发真正开始时才打开文件句柄 ⇒ 未登记对端时**不会**留下
// 一堆打开的临时文件句柄。
func deliveryFilesOf(files []*multipart.FileHeader) []federation.DeliveryFile {
	out := make([]federation.DeliveryFile, 0, len(files))
	for _, fh := range files {
		fh := fh
		out = append(out, federation.DeliveryFile{
			Name: fh.Filename,
			Size: fh.Size,
			Open: func() (io.ReadCloser, error) { return fh.Open() },
		})
	}
	return out
}

// deliveryFromName 发起柜的展示名（传给对方柜做「来自谁」的提示）。
func deliveryFromName() string {
	if cfg := conf.Snapshot(); cfg != nil {
		if name := strings.TrimSpace(cfg.App.Name); name != "" {
			return name
		}
	}
	return "PigeonBox"
}

// firstFormValue 取表单字段的第一个值（空列表返回空串）。
func firstFormValue(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}
