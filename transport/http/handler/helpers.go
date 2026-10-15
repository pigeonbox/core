// helpers.go handler 包内样板收口：登录守卫 / 密码守卫 / 路径 ID 解析 /
// multipart 上传落盘循环。此前同一模式散落 share_multi / localimport /
// localfiles / file_request / share_user / notify_user / admin_manage 各写一份，
// 策略调整需逐处同步——统一到本文件单一实现（响应文案/状态码与原各处一致）。
package handler

import (
	"context"
	"errors"
	"fmt"
	"mime/multipart"
	"strconv"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/pigeonbox/contracts/errcode"
	shareService "github.com/pigeonbox/core/app/share"
	"github.com/pigeonbox/core/pkg/resp"
	"github.com/pigeonbox/core/pkg/utils"
	"github.com/pigeonbox/core/storage"
)

// requireLogin 统一登录守卫：未登录写 401 响应并返回 ok=false。
func requireLogin(c *app.RequestContext) (uint, bool) {
	uid, ok := userIDFromCtx(c)
	if !ok {
		c.JSON(consts.StatusUnauthorized, map[string]interface{}{"code": errcode.CodeUnauthorized, "message": "未登录"})
	}
	return uid, ok
}

// resolveSharePassword 统一密码守卫：requireAuth=true 时哈希明文密码。
// 守卫失败已写响应（400 缺密码 / 500 哈希失败），调用方直接 return。
func resolveSharePassword(c *app.RequestContext, requireAuth bool, password string) (hash string, ok bool) {
	hash, err := utils.ResolveSharePassword(requireAuth, password)
	if err == nil {
		return hash, true
	}
	if errors.Is(err, utils.ErrPasswordRequired) {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{
			"code": 400, "message": "开启密码保护时必须提供密码",
		})
		return "", false
	}
	c.JSON(consts.StatusInternalServerError, map[string]interface{}{
		"code": 500, "message": "密码处理失败",
	})
	return "", false
}

// paramID 解析路径参数 :id（失败写 400 响应并返回 ok=false）。
func paramID(c *app.RequestContext, label string) (uint, bool) {
	id64, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		resp.NewErrorWithMessage(c, 10001, label+" ID 格式错误")
		return 0, false
	}
	return uint(id64), true
}

// parsePage 分页查询参数解析（缺失/非法回退 1/20；上限截断由 dao clampPage 统一兜底）。
func parsePage(c *app.RequestContext) (page, pageSize int) {
	page, _ = strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ = strconv.Atoi(c.DefaultQuery("page_size", "20"))
	return
}

// respondLegacyPage 管理端分页的历史约定包络：前端判 code===200 且读
// data.items/total（与 resp.Success 的 code=0 体系不同）——形状是既有前端
// 契约，只收敛调用点、不改线上响应体。
func respondLegacyPage(c *app.RequestContext, items any, total int64, page, pageSize int) {
	c.JSON(consts.StatusOK, map[string]interface{}{
		"code":    200,
		"message": "success",
		"data": map[string]interface{}{
			"items":     items,
			"total":     total,
			"page":      page,
			"page_size": pageSize,
		},
	})
}

// saveUploadEntries 校验并落盘 multipart 文件列表（多文件直传/访客投递共用）。
// 逐文件：文件名消毒 → 扩展名白名单 → 单文件大小上限 → 魔数一致性
// （encrypted=true 跳过：E2E 密文头为随机字节，必然不过魔数表）→ 唯一相对
// 路径落盘。任一失败：回滚已落盘文件、写错误响应并返回 ok=false。
func saveUploadEntries(ctx context.Context, c *app.RequestContext, st storage.StorageInterface, files []*multipart.FileHeader, encrypted bool) ([]shareService.StoredFileEntry, bool) {
	var stored []shareService.StoredFileEntry
	reject := func(bizCode int, msg string) {
		cleanupStored(ctx, st, stored)
		c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": bizCode, "message": msg})
	}
	for _, fh := range files {
		name := utils.SanitizeFileName(fh.Filename)
		if !utils.IsAllowedExtension(name) {
			reject(errcode.CodeFileTypeDenied, fmt.Sprintf("文件类型禁止上传: %s", name))
			return nil, false
		}
		if maxFile := utils.GetMaxFileSize(); maxFile > 0 && fh.Size > maxFile {
			reject(errcode.CodeTooLarge, fmt.Sprintf("文件 %s 超过单文件大小上限", name))
			return nil, false
		}
		// 魔数 + 扩展名一致性（读 512 字节头部；ReadCloser 不保证可 Seek）
		if !encrypted {
			var head []byte
			if f, oerr := fh.Open(); oerr == nil {
				head = readHead512(f)
				_ = f.Close()
			}
			if err := utils.CheckUploadContent(name, head); err != nil {
				reject(errcode.CodeFileTypeDenied, err.Error())
				return nil, false
			}
		}

		_, rel := utils.NewUploadRelPath(name)
		result, serr := st.SaveFile(ctx, fh, rel)
		if serr != nil {
			cleanupStored(ctx, st, stored)
			c.JSON(consts.StatusInternalServerError, map[string]interface{}{
				"code": 500, "message": fmt.Sprintf("文件保存失败: %v", serr),
			})
			return nil, false
		}
		stored = append(stored, shareService.StoredFileEntry{
			RelPath:  result.FilePath,
			FileName: name,
			Size:     result.FileSize,
			FileHash: result.FileHash,
		})
	}
	return stored, true
}
