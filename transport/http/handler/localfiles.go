// 管理端本地文件管理端点（对标上游 2.7.0 data/local 管理；AdminMiddleware 组内）。
// 路径以「root 索引 + 相对路径」表达，服务端配置为唯一真相源，杜绝穿越。
package handler

import (
	"context"
	"github.com/pigeonbox/contracts/errcode"
	"strconv"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	shareService "github.com/pigeonbox/core/app/share"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/pkg/resp"
	"github.com/pigeonbox/core/pkg/transfer"
	"github.com/pigeonbox/core/pkg/utils"
)

func localRootQuery(c *app.RequestContext) int {
	if v, err := strconv.Atoi(c.Query("root")); err == nil {
		return v
	}
	return 0
}

// AdminListLocalFiles GET /admin/local-files?root=0&dir=sub/dir
// 返回 {roots: [...], entries: [...]}（roots 供前端切换根目录）
func AdminListLocalFiles(ctx context.Context, c *app.RequestContext) {
	if shareSvc == nil {
		c.JSON(consts.StatusInternalServerError, map[string]interface{}{"code": errcode.CodeInternal, "message": "share service 未就绪"})
		return
	}
	entries, err := getShareService().ListLocalFiles(localRootQuery(c), c.Query("dir"))
	if err != nil {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": errcode.CodeInvalidParam, "message": err.Error()})
		return
	}
	resp.Success(c, map[string]interface{}{
		"roots":   shareService.LocalImportRoots(),
		"entries": entries,
	})
}

// AdminDeleteLocalFile DELETE /admin/local-files?root=0&path=a/b.txt
func AdminDeleteLocalFile(ctx context.Context, c *app.RequestContext) {
	if shareSvc == nil {
		c.JSON(consts.StatusInternalServerError, map[string]interface{}{"code": errcode.CodeInternal, "message": "share service 未就绪"})
		return
	}
	path := c.Query("path")
	if path == "" {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": errcode.CodeInvalidParam, "message": "缺少 path"})
		return
	}
	if err := getShareService().DeleteLocalFile(localRootQuery(c), path); err != nil {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": errcode.CodeInvalidParam, "message": err.Error()})
		return
	}
	resp.SuccessWithMessage(c, "删除成功", nil)
}

// AdminImportLocalFile POST /admin/local-files/import
// 请求体：{root, path, expire_value, expire_style, require_auth, password, custom_code}
// 管理员把白名单目录内的文件直接生成提取码（服务端拷贝入存储，配额/审核同链路）。
func AdminImportLocalFile(ctx context.Context, c *app.RequestContext) {
	if shareSvc == nil {
		c.JSON(consts.StatusInternalServerError, map[string]interface{}{"code": errcode.CodeInternal, "message": "share service 未就绪"})
		return
	}
	var body struct {
		Root        int    `json:"root"`
		Path        string `json:"path"`
		ExpireValue int    `json:"expire_value"`
		ExpireStyle string `json:"expire_style"`
		RequireAuth bool   `json:"require_auth"`
		Password    string `json:"password"`
		CustomCode  string `json:"custom_code"`
	}
	if err := c.BindJSON(&body); err != nil || body.Path == "" {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": errcode.CodeInvalidParam, "message": "请求体无效（需 root+path）"})
		return
	}
	if body.ExpireStyle == "" {
		body.ExpireStyle = "day"
		if body.ExpireValue <= 0 {
			body.ExpireValue = 7
		}
	}
	if err := utils.CheckExpireStyleAllowed(body.ExpireStyle); err != nil {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": errcode.CodeInvalidParam, "message": err.Error()})
		return
	}
	passwordHash, ok := resolveSharePassword(c, body.RequireAuth, body.Password)
	if !ok {
		return
	}
	absPath, err := shareService.LocalImportAbsPath(body.Root, body.Path)
	if err != nil {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": errcode.CodeInvalidParam, "message": err.Error()})
		return
	}
	result, err := getShareService().ImportLocalFile(ctx, shareService.ImportLocalOpts{
		AbsPath:      absPath,
		ExpireValue:  body.ExpireValue,
		ExpireStyle:  body.ExpireStyle,
		RequireAuth:  body.RequireAuth,
		PasswordHash: passwordHash,
		CustomCode:   body.CustomCode,
		OwnerIP:      middleware.ClientIP(c),
	})
	if err != nil {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": errcode.CodeInvalidParam, "message": err.Error()})
		return
	}

	transfer.Record(transfer.Entry{
		Operation: transfer.OpUpload, FileCodeID: result.ID, Code: result.Code,
		FileName: result.Text, FileSize: result.Size,
		Username: middleware.UsernameFromContext(ctx), IP: middleware.ClientIP(c),
	})

	c.JSON(consts.StatusOK, map[string]interface{}{
		"code":    0,
		"message": "导入成功",
		"data": map[string]interface{}{
			"code":      result.Code,
			"share_url": result.FullShareURL,
		},
	})
}
