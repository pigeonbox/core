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
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/oekit/pkg/pathx"
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
			"code": errcode.CodeInvalidParam, "message": "开启密码保护时必须提供密码",
		})
		return "", false
	}
	c.JSON(consts.StatusInternalServerError, map[string]interface{}{
		"code": errcode.CodeInternal, "message": "密码处理失败",
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

// maxShareRelDirRunes 分享内相对目录长度上限（字符数）。
// 取 200 而非列宽 255：MySQL 的 varchar(255) 按**字符**计，但 sqlite 侧是 TEXT
// 不截断 ⇒ 一旦超过列宽，只有 MySQL 部署会炸，属于「本地全绿、上线才红」的形态。
const maxShareRelDirRunes = 200

// sanitizeShareRelDir 分享内相对目录消毒（空串 = 根）。
//
// 入参源自浏览器的 webkitRelativePath，**完全客户端可控**，因此与 object_key
// 同一严格档：pathx.SanitizeRelStrict 统一把 "" 当分隔符，并拒绝绝对路径 /
// UNC / Windows 盘符 / 显式 ".." 段。
//
// 为什么这里必须严格：这些目录段最终会进 zip 条目名（zipPath）⇒ 放过一个 ".."
// 就是 Zip Slip：用户解压时文件被写到压缩包外的目录里。
//
// 超长直接拒绝而不是截断：截断会把两个不同目录折叠成同一个，用户看到的是
// 「我的文件跑到别的目录里去了」，比明确报错难查得多。
func sanitizeShareRelDir(raw string) (string, error) {
	dir := strings.TrimSpace(raw)
	if dir == "" {
		return "", nil
	}
	clean, ok := pathx.SanitizeRelStrict(dir)
	if !ok {
		return "", fmt.Errorf("目录路径非法（不允许绝对路径/盘符/上跳段）: %s", dir)
	}
	if n := len([]rune(clean)); n > maxShareRelDirRunes {
		return "", fmt.Errorf("目录路径过长（%d 字符，上限 %d）: %s", n, maxShareRelDirRunes, clean)
	}
	return clean, nil
}

// saveUploadEntries 校验并落盘 multipart 文件列表（多文件直传/访客投递共用）。
// 逐文件：文件名消毒 → 扩展名白名单 → 单文件大小上限 → 魔数一致性
// （encrypted=true 跳过：E2E 密文头为随机字节，必然不过魔数表）→ 唯一相对
// 路径落盘。任一失败：回滚已落盘文件、写错误响应并返回 ok=false。
func saveUploadEntries(ctx context.Context, c *app.RequestContext, st storage.StorageInterface, files []*multipart.FileHeader, dirs []string, encrypted bool) ([]shareService.StoredFileEntry, bool) {
	var stored []shareService.StoredFileEntry
	reject := func(bizCode int, msg string) {
		cleanupStored(ctx, st, stored)
		c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": bizCode, "message": msg})
	}

	// 目录上传（5.3.0）：dirs 与 files **同序**，缺省（nil）表示扁平上传。
	//
	// 数量不一致直接拒绝而不是「尽量对齐」：错位一格会把每个文件塞进隔壁的
	// 目录，而落库结果看起来完全正常（没有任何报错线索可回查）。
	relDirs := make([]string, len(files))
	if len(dirs) > 0 {
		if len(dirs) != len(files) {
			c.JSON(consts.StatusBadRequest, map[string]interface{}{
				"code":    errcode.CodeInvalidParam,
				"message": fmt.Sprintf("dirs 与 files 数量不一致（%d vs %d）", len(dirs), len(files)),
			})
			return nil, false
		}
		for i, d := range dirs {
			clean, derr := sanitizeShareRelDir(d)
			if derr != nil {
				c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": errcode.CodeInvalidParam, "message": derr.Error()})
				return nil, false
			}
			relDirs[i] = clean
		}
	}

	for i, fh := range files {
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
				"code": errcode.CodeInternal, "message": fmt.Sprintf("文件保存失败: %v", serr),
			})
			return nil, false
		}
		stored = append(stored, shareService.StoredFileEntry{
			RelPath:  result.FilePath,
			FileName: name,
			RelDir:   relDirs[i],
			Size:     result.FileSize,
			FileHash: result.FileHash,
		})
	}
	return stored, true
}
