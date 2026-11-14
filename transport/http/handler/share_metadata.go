// 取件元数据端点（GET /share/metadata/:code，手写路由）。
// 查询不扣次数、不要求密码；限流走 download 桶（2026-10-08 起未归桶路径另有全局默认桶兜底），
// 存在性探测面由 6 位随机码空间 + 统一 404 语义约束。
package handler

import (
	"context"
	"errors"
	"strconv"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/pigeonbox/contracts/errcode"
	"gorm.io/gorm"

	shareService "github.com/pigeonbox/core/app/share"
	"github.com/pigeonbox/core/pkg/gate"
	"github.com/pigeonbox/core/pkg/middleware"
	"github.com/pigeonbox/core/pkg/resp"
)

// ShareMetadata 元数据查询：供取件页渲染文件名/大小/有效期/剩余次数
func ShareMetadata(ctx context.Context, c *app.RequestContext) {
	if shareSvc == nil {
		c.JSON(consts.StatusInternalServerError, map[string]interface{}{"code": errcode.CodeInternal, "message": "share service 未就绪"})
		return
	}
	code := c.Param("code")
	if code == "" {
		c.JSON(consts.StatusBadRequest, map[string]interface{}{"code": errcode.CodeInvalidParam, "message": "缺少取件码"})
		return
	}
	// 入口预检（与 /anonymous/retrieve 同口径，core#163）：本面此前无预检无记账
	// ⇒ 枚举只剩 QPS 限流桶（50/s vs 锁定 10 次/300s，~4500× 吞吐差）。
	// 预检含按 IP 的枚举维度（见 gate.PickupLockedRemain）。
	if remain, locked := gate.PickupLockedRemain(ctx, middleware.ClientIP(c), code); locked {
		c.JSON(consts.StatusTooManyRequests, map[string]interface{}{
			"code":    errcode.CodeTooManyAttempts,
			"message": "尝试过于频繁，已临时锁定，请 " + strconv.Itoa(remain) + " 秒后重试",
		})
		return
	}
	meta, err := getShareService().GetShareMetadata(ctx, code)
	if err != nil {
		// 占位分享（文件未上传）：与 select/download/preview 面同码同文案（20001）
		if errors.Is(err, shareService.ErrShareNotReady) {
			c.JSON(consts.StatusNotFound, map[string]interface{}{"code": errcode.CodeShareNotFound, "message": "分享未完成上传，暂时无法取件"})
			return
		}
		// 取件次数已用完：20004「超过取件次数」（2026-10-24）——此前混在
		// 「不存在/已过期」的 404 里（IsExpired 当年含次数语义）。
		if errors.Is(err, shareService.ErrShareCountExhausted) {
			c.JSON(consts.StatusForbidden, map[string]interface{}{"code": errcode.CodeShareReachLimit, "message": "超过取件次数"})
			return
		}
		// 枚举防护记账（core#163）：只在「码不存在」记账——与 retrieve 的
		// ErrCodeNotFound 分支同口径；已过期/被封禁/次数耗尽/基建故障不记，
		// 避免正常用户刷新失效分享页撞锁（也避免 DB 故障期把全网记成锁定）。
		// 码不存在 = ResolveByCode 底层 gorm.ErrRecordNotFound；
		// 过期（"file has expired"）与封禁（ShareBlockedError）是解析成功后的
		// 另一类错误，不会命中本判定。
		if errors.Is(err, gorm.ErrRecordNotFound) {
			gate.RecordPickupFailure(ctx, middleware.ClientIP(c), code)
		}
		// 不区分「不存在/已过期/被封禁」，统一 404 降低探测面
		c.JSON(consts.StatusNotFound, map[string]interface{}{"code": errcode.CodeNotFound, "message": "分享不存在或已过期"})
		return
	}
	resp.Success(c, meta)
}
