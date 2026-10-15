// 多文件分享创建端点（P0 多文件，手写路由不走 IDL）：
//
//	POST /api/v1/share/multi-direct  multipart 多文件直传（小文件）
//	POST /api/v1/share/multi-bind    绑定已上传的 chunk 会话/presign 对象为一个多文件分享
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/pigeonbox/contracts/errcode"
	"github.com/pigeonbox/core/app/chunk"
	shareService "github.com/pigeonbox/core/app/share"
	"github.com/pigeonbox/core/pkg/gate"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/pkg/resp"
	"github.com/pigeonbox/core/pkg/transfer"
	"github.com/pigeonbox/core/pkg/utils"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/pigeonbox/core/storage"
)

// userIDAny 读请求身份：JWT 走 c.Set（OptionalAuthMiddleware），API Key 走 ctx 值
// （validateAPIKey newCtx）。两条注入路径并存，必须都查（回归：此前只查 ctx，
// JWT 用户的 custom_code/配额归属全部静默失效）。
func userIDAny(ctx context.Context, c *app.RequestContext) (uint, bool) {
	if v, ok := middleware.UserIDFromContext(ctx); ok {
		return v, true
	}
	return userIDFromCtx(c)
}

var multiChunkSvcInstance *chunk.Service

// SetMultiChunkService 注入多文件绑定用 chunk 会话查询服务（bootstrap 调用）。
func SetMultiChunkService(s *chunk.Service) { multiChunkSvcInstance = s }

// multiChunkSvc 多文件绑定的 chunk 会话查询
func multiChunkSvc() *chunk.Service {
	if multiChunkSvcInstance == nil {
		// fail-fast：装配缺失必须显式暴露（bootstrap 保证先于服务流量注入）。
		panic("chunk service not initialized: SetMultiChunkService must be called before serving")
	}
	return multiChunkSvcInstance
}

// multiCommonParams 多文件创建的公共表单/JSON 参数
type multiCommonParams struct {
	ExpireValue  int
	ExpireStyle  string
	RequireAuth  bool
	PasswordHash string
	Encrypted    bool   // E2E：客户端已加密（跳过魔数复检，密文头为随机字节）
	CustomCode   string // 自定义取件码（P3；登录用户专属）
	Note         string // 文件备注（可空，取件页展示；2026-10-12 #489）
	UserID       *uint
	UploadType   string
	OwnerIP      string
}

// parseMultiCommon 解析过期/密码/身份参数（JSON body 版本）。
// 返回 nil 表示已写出错误响应。
func parseMultiCommonFromJSON(ctx context.Context, c *app.RequestContext) *multiCommonParams {
	var body struct {
		ExpireValue  int    `json:"expire_value"`
		ExpireStyle  string `json:"expire_style"`
		RequireAuth  bool   `json:"require_auth"`
		Password     string `json:"password"`
		Encrypted    bool   `json:"encrypted"`
		CustomCode   string `json:"custom_code"`
		Note         string `json:"note"`
		EntriesEmpty bool   `json:"-"`
	}
	if err := json.Unmarshal(c.Request.Body(), &body); err != nil {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{
			"code":    errcode.CodeInvalidParam,
			"message": "请求体解析失败: " + err.Error(),
		})
		return nil
	}
	return finishMultiCommon(ctx, c, body.ExpireValue, body.ExpireStyle, body.RequireAuth, body.Password, body.Encrypted, body.CustomCode, body.Note)
}

// parseMultiCommonFromForm multipart 表单版本
func parseMultiCommonFromForm(ctx context.Context, c *app.RequestContext) *multiCommonParams {
	expireValue, _ := strconv.Atoi(c.DefaultPostForm("expire_value", "1"))
	expireStyle := c.DefaultPostForm("expire_style", "day")
	requireAuth := c.DefaultPostForm("require_auth", "false") == "true"
	password := c.DefaultPostForm("password", "")
	encrypted := c.DefaultPostForm("encrypted", "false") == "true"
	customCode := c.DefaultPostForm("custom_code", "")
	note := c.DefaultPostForm("note", "")
	return finishMultiCommon(ctx, c, expireValue, expireStyle, requireAuth, password, encrypted, customCode, note)
}

// finishMultiCommon 公共参数收尾：样式白名单/密码哈希/身份
func finishMultiCommon(ctx context.Context, c *app.RequestContext, expireValue int, expireStyle string, requireAuth bool, password string, encrypted bool, customCode string, note string) *multiCommonParams {
	if err := utils.CheckExpireStyleAllowed(expireStyle); err != nil {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{
			"code":    errcode.CodeInvalidParam,
			"message": err.Error(),
		})
		return nil
	}
	p := &multiCommonParams{
		ExpireValue: expireValue,
		ExpireStyle: expireStyle,
		RequireAuth: requireAuth,
		Encrypted:   encrypted,
		Note:        note,
		OwnerIP:     middleware.ClientIP(c),
	}
	if hash, ok := resolveSharePassword(c, requireAuth, password); ok {
		p.PasswordHash = hash
	} else {
		return nil
	}
	if v, ok := userIDAny(ctx, c); ok {
		p.UserID = &v
		p.UploadType = "authenticated"
		// 自定义取件码（P3）：仅登录用户可指定（防匿名抢注）
		p.CustomCode = customCode
	} else {
		p.UploadType = "anonymous"
	}
	return p
}

// multiExpire 通用参数 → 过期字段
func (p *multiCommonParams) multiExpire() (*time.Time, int) {
	return utils.CalculateExpireTime(p.ExpireValue, p.ExpireStyle),
		utils.CalculateExpireCount(p.ExpireStyle, p.ExpireValue)
}

// newRelPath 新存储相对路径（uploads/YYYY/MM/DD/uuid.ext，与单文件直传一致；
// 收口 utils.NewUploadRelPath，单次取时钟避免跨秒日期漂移）
func newRelPath(originalName string) (uuidName, rel string) {
	return utils.NewUploadRelPath(originalName)
}

// cleanupStored 失败回滚：删除已落存储的文件（best-effort）
func cleanupStored(ctx context.Context, st storage.StorageInterface, entries []shareService.StoredFileEntry) {
	if st == nil {
		return
	}
	for _, e := range entries {
		_ = st.DeleteFile(ctx, e.RelPath)
	}
}

// ==================== 多文件直传（multipart）====================

// MultiShareDirect 多文件直传建分享。
// @router /api/v1/share/multi-direct [POST]
func MultiShareDirect(ctx context.Context, c *app.RequestContext) {
	if shareSvc == nil {
		c.JSON(consts.StatusInternalServerError, map[string]interface{}{"code": errcode.CodeInternal, "message": "share service 未就绪"})
		return
	}
	form, err := c.MultipartForm()
	if err != nil {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": errcode.CodeInvalidParam, "message": "解析 multipart 失败: " + err.Error()})
		return
	}
	files := form.File["files"]
	if len(files) == 0 {
		files = form.File["file"] // 兼容单文件字段名
	}
	if len(files) == 0 {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": errcode.CodeInvalidParam, "message": "请上传文件"})
		return
	}
	if len(files) > 100 {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": errcode.CodeInvalidParam, "message": "单次分享文件数超过上限（100 个）"})
		return
	}

	p := parseMultiCommonFromForm(ctx, c)
	if p == nil {
		return
	}

	// 上传闸门 + 匿名日配额（合计一次校验，先于落盘——治理回归要点）
	var gateUserID *uint
	if v, ok := userIDAny(ctx, c); ok {
		gateUserID = &v
	}
	if err := gate.CheckUploadAllowed(gateUserID); err != nil {
		resp.NewTypedError(c, err)
		return
	}
	if err := gate.CheckUploadLogin(gateUserID); err != nil {
		resp.NewTypedError(c, err)
		return
	}
	var totalDeclared int64
	for _, f := range files {
		totalDeclared += f.Size
	}
	if gateUserID == nil {
		if err := gate.CheckAnonymousQuota(ctx, middleware.ClientIP(c), totalDeclared); err != nil {
			resp.NewTypedError(c, err)
			return
		}
	}

	st := manageStorage
	stored, ok := saveUploadEntries(ctx, c, st, files, p.Encrypted)
	if !ok {
		return
	}

	expireAt, expireCount := p.multiExpire()
	shareResult, err := shareSvc.CreateMultiFileShare(ctx, &shareService.MultiShareReq{
		Entries:      stored,
		ExpiredAt:    expireAt,
		ExpiredCount: expireCount,
		RequireAuth:  p.RequireAuth,
		PasswordHash: p.PasswordHash,
		UserID:       p.UserID,
		UploadType:   p.UploadType,
		OwnerIP:      p.OwnerIP,
		Channel:      "direct",
		Encrypted:    p.Encrypted,
		CustomCode:   p.CustomCode,
		Note:         p.Note,
	})
	if err != nil {
		cleanupStored(ctx, st, stored)
		resp.NewTypedError(c, err)
		return
	}

	transfer.Record(transfer.Entry{
		Operation: transfer.OpUpload, FileCodeID: shareResult.ID, Code: shareResult.Code,
		FileName: fmt.Sprintf("%d files", len(stored)), FileSize: shareResult.Size,
		UserID: p.UserID, APIKeyID: apiKeyIDPtr(ctx),
		Username: middleware.UsernameFromContext(ctx), IP: p.OwnerIP,
	})

	c.JSON(consts.StatusOK, map[string]interface{}{
		"code":    0,
		"message": "文件上传成功",
		"data": map[string]interface{}{
			"code":        shareResult.Code,
			"url":         shareResult.FullShareURL,
			"share_url":   fmt.Sprintf("/share/%s", shareResult.Code),
			"file_count":  len(stored),
			"pickup_code": shareResult.PickupCode,
		},
	})
}

// ==================== 多文件绑定（chunk 会话 / presign 对象）====================

// MultiShareBind 把多个已上传的 chunk 会话（或 presign 对象 key）绑定为
// 一个多文件分享。请求体：
//
//	{
//	  "entries": [{"upload_id": "..."} | {"object_key": "...", "file_name": "..."}],
//	  "expire_value": 1, "expire_style": "day", "require_auth": false, "password": ""
//	}
//
// @router /api/v1/share/multi-bind [POST]
func MultiShareBind(ctx context.Context, c *app.RequestContext) {
	if shareSvc == nil {
		c.JSON(consts.StatusInternalServerError, map[string]interface{}{"code": errcode.CodeInternal, "message": "share service 未就绪"})
		return
	}
	var body struct {
		Entries []struct {
			UploadID  string `json:"upload_id"`
			ObjectKey string `json:"object_key"`
			FileName  string `json:"file_name"`
		} `json:"entries"`
		ExpireValue int    `json:"expire_value"`
		ExpireStyle string `json:"expire_style"`
		RequireAuth bool   `json:"require_auth"`
		Password    string `json:"password"`
		Encrypted   bool   `json:"encrypted"`
	}
	if err := json.Unmarshal(c.Request.Body(), &body); err != nil {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": errcode.CodeInvalidParam, "message": "请求体解析失败: " + err.Error()})
		return
	}
	if len(body.Entries) == 0 || len(body.Entries) > 100 {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": errcode.CodeInvalidParam, "message": "entries 需为 1-100 项"})
		return
	}
	p := parseMultiCommonFromJSON(ctx, c)
	if p == nil {
		return
	}
	// E2E（multi-bind 的 encrypted 已在 parseMultiCommonFromJSON 读入 p.Encrypted）
	_ = body.Encrypted

	var gateUserID *uint
	if v, ok := userIDAny(ctx, c); ok {
		gateUserID = &v
	}
	if err := gate.CheckUploadAllowed(gateUserID); err != nil {
		resp.NewTypedError(c, err)
		return
	}
	if err := gate.CheckUploadLogin(gateUserID); err != nil {
		resp.NewTypedError(c, err)
		return
	}

	st := manageStorage
	chunkSvc := multiChunkSvc()
	bound := make([]shareService.StoredFileEntry, 0, len(body.Entries))
	mergedUploadIDs := make([]string, 0, len(body.Entries))

	// 第一遍：收集申报大小做匿名配额预检（fail-fast，先于合并落盘）
	var declared int64
	for _, e := range body.Entries {
		if e.UploadID == "" && e.ObjectKey == "" {
			c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": errcode.CodeInvalidParam, "message": "entry 缺少 upload_id/object_key"})
			return
		}
		if info, gerr := chunkSvc.GetUploadInfo(ctx, e.UploadID); e.UploadID != "" && gerr == nil {
			declared += info.FileSize
		}
	}
	if gateUserID == nil {
		if err := gate.CheckAnonymousQuota(ctx, p.OwnerIP, declared); err != nil {
			resp.NewTypedError(c, err)
			return
		}
	}

	for _, e := range body.Entries {
		switch {
		case e.UploadID != "":
			entry, uploadID, ok := bindChunkEntry(ctx, c, chunkSvc, st, e.UploadID, p.Encrypted)
			if !ok {
				cleanupStored(ctx, st, bound)
				for _, id := range mergedUploadIDs {
					_ = st.CleanChunks(ctx, id)
				}
				return
			}
			bound = append(bound, *entry)
			if uploadID != "" {
				mergedUploadIDs = append(mergedUploadIDs, uploadID)
			}
		case e.ObjectKey != "":
			// object_key 投毒堵口（2026-10-05 审计 P0）：object_key 是客户端可控、
			// 直接入库为 RelPath 的存储相对路径，此前仅验 GetFileSize 存在——
			// 传 `../../etc/passwd` 即可把任意可读文件绑成可下载分享。
			// 现仅接受服务端生成命名空间 uploads/（presign genObjectKey 布局），
			// 且清洗后不得逃逸存储根（存储层 localContain 兜底双保险）。
			cleanKey := path.Clean(filepath.ToSlash(e.ObjectKey))
			if !strings.HasPrefix(cleanKey, "uploads/") || strings.Contains(cleanKey, "..") {
				cleanupStored(ctx, st, bound)
				c.JSON(consts.StatusBadRequest, map[string]interface{}{
					"code":    errcode.CodeInvalidParam,
					"message": fmt.Sprintf("object_key 非法（仅接受 uploads/ 下服务端生成的对象）: %s", e.ObjectKey),
				})
				return
			}
			name := utils.SanitizeFileName(e.FileName)
			if name == "" || !utils.IsAllowedExtension(name) {
				cleanupStored(ctx, st, bound)
				c.JSON(consts.StatusBadRequest, map[string]interface{}{
					"code":    errcode.CodeFileTypeDenied,
					"message": fmt.Sprintf("文件类型禁止上传: %s", e.FileName),
				})
				return
			}
			size, serr := st.GetFileSize(ctx, e.ObjectKey)
			if serr != nil || size <= 0 {
				cleanupStored(ctx, st, bound)
				c.JSON(consts.StatusBadRequest, map[string]interface{}{
					"code":    errcode.CodeInvalidParam,
					"message": fmt.Sprintf("对象不存在或大小异常: %s", e.ObjectKey),
				})
				return
			}
			bound = append(bound, shareService.StoredFileEntry{
				RelPath:  e.ObjectKey,
				FileName: name,
				Size:     size,
			})
		}
	}

	expireAt, expireCount := p.multiExpire()
	shareResult, err := shareSvc.CreateMultiFileShare(ctx, &shareService.MultiShareReq{
		Entries:      bound,
		ExpiredAt:    expireAt,
		ExpiredCount: expireCount,
		RequireAuth:  p.RequireAuth,
		PasswordHash: p.PasswordHash,
		UserID:       p.UserID,
		UploadType:   p.UploadType,
		OwnerIP:      p.OwnerIP,
		Channel:      "chunk",
		Encrypted:    p.Encrypted,
		CustomCode:   p.CustomCode,
		Note:         p.Note,
	})
	if err != nil {
		cleanupStored(ctx, st, bound)
		for _, id := range mergedUploadIDs {
			_ = st.CleanChunks(ctx, id)
		}
		resp.NewTypedError(c, err)
		return
	}
	// 分享创建成功后清理分片临时目录
	for _, id := range mergedUploadIDs {
		_ = st.CleanChunks(ctx, id)
	}

	transfer.Record(transfer.Entry{
		Operation: transfer.OpUpload, FileCodeID: shareResult.ID, Code: shareResult.Code,
		FileName: fmt.Sprintf("%d files", len(bound)), FileSize: shareResult.Size,
		UserID: p.UserID, APIKeyID: apiKeyIDPtr(ctx),
		Username: middleware.UsernameFromContext(ctx), IP: p.OwnerIP,
	})

	c.JSON(consts.StatusOK, map[string]interface{}{
		"code":    0,
		"message": "分享创建成功",
		"data": map[string]interface{}{
			"code":        shareResult.Code,
			"url":         shareResult.FullShareURL,
			"share_url":   fmt.Sprintf("/share/%s", shareResult.Code),
			"file_count":  len(bound),
			"pickup_code": shareResult.PickupCode,
		},
	})
}

// bindChunkEntry 校验归属 → 分片齐全 → 合并落盘 → 哈希/魔数/大小复核。
// encrypted=true 时跳过魔数复检（密文头为随机字节）。
// ok=false 时已写出错误响应（调用方统一回滚）。
func bindChunkEntry(ctx context.Context, c *app.RequestContext, chunkSvc *chunk.Service, st storage.StorageInterface, uploadID string, encrypted bool) (*shareService.StoredFileEntry, string, bool) {
	info, err := chunkSvc.GetUploadInfo(ctx, uploadID)
	if err != nil {
		c.JSON(consts.StatusNotFound, map[string]interface{}{"code": errcode.CodeNotFound, "message": "上传记录不存在: " + uploadID})
		return nil, "", false
	}

	// 归属校验（与 chunk Complete 同策略）：IP 一致 / 同一登录用户 / 会话令牌
	if !multiOwnedByCaller(ctx, info, c) {
		c.JSON(consts.StatusForbidden, map[string]interface{}{"code": errcode.CodeForbidden, "message": "无权操作该上传会话"})
		return nil, "", false
	}

	indexes, err := chunkSvc.GetUploadedChunkIndexes(ctx, uploadID)
	if err != nil || len(indexes) < info.TotalChunks {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{
			"code":    errcode.CodeInvalidParam,
			"name":    info.FileName,
			"message": fmt.Sprintf("分片未传完（%d/%d）", len(indexes), info.TotalChunks),
		})
		return nil, "", false
	}

	_, rel := newRelPath(info.FileName)
	if err := st.MergeChunks(ctx, uploadID, info.TotalChunks, rel); err != nil {
		c.JSON(consts.StatusInternalServerError, map[string]interface{}{"code": errcode.CodeInternal, "message": "合并分片失败: " + err.Error()})
		return nil, "", false
	}

	// 哈希 + 魔数 + 大小复核（与 chunk Complete 一致）
	fileHash := ""
	var actualSize int64
	var head []byte
	if rc, fsize, rerr := st.GetFileReader(ctx, rel); rerr == nil {
		actualSize = fsize
		head = readHead512(rc)
		fileHash, _ = utils.HashReader(io.MultiReader(bytes.NewReader(head), rc))
		_ = rc.Close()
	}
	if !encrypted && head != nil {
		if err := utils.CheckUploadContent(info.FileName, head); err != nil {
			_ = st.DeleteFile(ctx, rel)
			c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": errcode.CodeFileTypeDenied, "message": err.Error()})
			return nil, "", false
		}
	}
	if actualSize > 0 {
		if maxFile := utils.GetMaxFileSize(); maxFile > 0 && actualSize > maxFile {
			_ = st.DeleteFile(ctx, rel)
			c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": errcode.CodeTooLarge, "message": fmt.Sprintf("合并后文件大小 %d 超过上限 %d", actualSize, maxFile)})
			return nil, "", false
		}
		if info.FileSize > 0 && actualSize != info.FileSize {
			_ = st.DeleteFile(ctx, rel)
			c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": errcode.CodeChunkInvalid, "message": fmt.Sprintf("合并后大小 %d 与申报 %d 不符", actualSize, info.FileSize)})
			return nil, "", false
		}
	}

	_ = chunkSvc.CompleteUpload(ctx, uploadID)
	return &shareService.StoredFileEntry{
		RelPath:  rel,
		FileName: utils.SanitizeFileName(info.FileName),
		Size:     actualSize,
		FileHash: fileHash,
	}, uploadID, true
}

// multiOwnedByCaller 绑定归属校验（薄委托 chunk.OwnedByCaller 单一实现）：
// 控制记录记有 OwnerIP 时，要求 IP 一致、同一登录用户或持有效会话令牌；
// 老数据（OwnerIP 为空）跳过保持兼容。
func multiOwnedByCaller(ctx context.Context, info *model.UploadChunk, c *app.RequestContext) bool {
	var callerUserID *uint
	if uid, ok := middleware.UserIDFromContext(ctx); ok {
		callerUserID = &uid
	}
	return chunk.OwnedByCaller(info, middleware.ClientIP(c), string(c.GetHeader("X-Upload-Token")), callerUserID)
}

// apiKeyIDPtr 提取 ctx 中的 API Key 归因（JWT/匿名请求返回 nil）
func apiKeyIDPtr(ctx context.Context) *uint {
	if id, ok := middleware.APIKeyIDFromContext(ctx); ok {
		return &id
	}
	return nil
}

// readHead512 读 512 字节头部（ReadCloser 不保证可 Seek；读失败返回 nil 跳过魔数复检）
func readHead512(rc io.ReadCloser) []byte {
	buf := make([]byte, 512)
	n, err := io.ReadFull(rc, buf)
	if err != nil && err != io.ErrUnexpectedEOF {
		return nil
	}
	return buf[:n]
}
