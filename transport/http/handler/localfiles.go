// 管理端本地文件管理端点（对标上游 2.7.0 data/local 管理；AdminMiddleware 组内）。
// 路径以「root 索引 + 相对路径」表达，服务端配置为唯一真相源，杜绝穿越。
package handler

import (
	"context"
	"fmt"
	"strconv"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/pigeonbox/contracts/errcode"

	shareService "github.com/pigeonbox/core/app/share"
	"github.com/pigeonbox/core/internal/paging"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/pkg/resp"
	"github.com/pigeonbox/core/pkg/transfer"
	"github.com/pigeonbox/core/pkg/utils"
)

// parseLocalRoot 解析查询参数 root（白名单根目录序号）——**严格解析**（core#113）。
//
// 旧行为：strconv.Atoi 失败**静默落 root 0** ⇒ DELETE /admin/local-files?root=2
// 里 root 打错（非数字/空格/URL 编码错）会被悄悄改成 root 0，删除/导入打到
// **另一个根目录**上的同名相对路径，接口照样回「删除成功」——破坏性端点 +
// 静默换目标，与 core#102「解析失败必须显式失败」同族。
//
// 口径（对齐 core#102/#104/#123 家族）：
//   - 缺省/空串 ⇒ 0（默认根，与「没传」同义，历史行为不动）；
//   - 非数字（含空格/小数/0x 形态）⇒ 400 + 10001 + 点名参数（paging.ParamError 形态）；
//   - 数字但越界（<0 或 ≥ 已配置根数）⇒ 400 + 10001 + 点名参数；
//   - roots 为空（功能未启用/未配置）不做越界判定，交服务端回「未启用」引导语。
//
// path 侧的穿越防护不在本函数：app/share.resolveLocalPath → pathx.SanitizeRelStrict
// 严格档（统一按反斜杠分隔符、拒绝对路径/UNC/盘符/显式 .. 段），EvalSymlinks 后
// 复核落点仍在根内。本函数只管 root 索引的解析口径。
func parseLocalRoot(c *app.RequestContext) (int, bool) {
	s := c.Query("root")
	if s == "" {
		return 0, true // 未传 ⇒ 默认根 0（历史口径不动）
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		perr := &paging.ParamError{Param: "root", Value: s, Expect: "非负整数（白名单根目录序号）"}
		resp.NewErrorWithMessage(c, errcode.CodeInvalidParam, perr.Error())
		return 0, false
	}
	if roots := shareService.LocalImportRoots(); len(roots) > 0 && (v < 0 || v >= len(roots)) {
		perr := &paging.ParamError{Param: "root", Value: s,
			Expect: fmt.Sprintf("0..%d 的整数（白名单根目录序号）", len(roots)-1)}
		resp.NewErrorWithMessage(c, errcode.CodeInvalidParam, perr.Error())
		return 0, false
	}
	return v, true
}

// AdminListLocalFiles GET /admin/local-files?root=0&dir=sub/dir
// 返回 {roots: [...], entries: [...]}（roots 供前端切换根目录）
func AdminListLocalFiles(ctx context.Context, c *app.RequestContext) {
	// root 严格解析前置于服务就绪检查：解析失败在触达服务前就 400 返回。
	root, ok := parseLocalRoot(c)
	if !ok {
		return
	}
	if shareSvc == nil {
		c.JSON(consts.StatusInternalServerError, map[string]interface{}{"code": errcode.CodeInternal, "message": "share service 未就绪"})
		return
	}
	entries, err := getShareService().ListLocalFiles(root, c.Query("dir"))
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
	// root 严格解析前置于服务就绪检查（core#113）：root 打错绝不能静默落到
	// root 0 —— 删除是破坏性端点，打错根目录 = 删错文件。
	root, ok := parseLocalRoot(c)
	if !ok {
		return
	}
	if shareSvc == nil {
		c.JSON(consts.StatusInternalServerError, map[string]interface{}{"code": errcode.CodeInternal, "message": "share service 未就绪"})
		return
	}
	path := c.Query("path")
	if path == "" {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": errcode.CodeInvalidParam, "message": "缺少 path"})
		return
	}
	if err := getShareService().DeleteLocalFile(root, path); err != nil {
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
			// 站点默认过期天数（transfer.expire_default，缺省 7）：此前硬编码 7，
			// 管理台「默认过期天数」对本通道形同虚设（2026-11-02 接线收口）。
			_, body.ExpireValue = utils.EffectiveTransferDefaults()
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
