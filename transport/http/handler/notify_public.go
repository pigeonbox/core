package handler

import (
	"context"
	"github.com/pigeonbox/contracts/errcode"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
)

// ListPublicNotifies 站级公告（匿名公开端点，对标上游首页通知条）。
// notify.Active 已限定 target_user_id 为空的全站公告；匿名可访问，
// 前端 SiteNotice 组件消费（可关闭，localStorage 记忆已读）。
func ListPublicNotifies(ctx context.Context, c *app.RequestContext) {
	items, err := getNotifyService().Active(ctx, "")
	if err != nil {
		c.JSON(consts.StatusInternalServerError, map[string]interface{}{"code": errcode.CodeInternal, "message": err.Error()})
		return
	}
	// 精简字段：公告条只需要展示信息（最多 50 条，前端按级别排序展示）。
	// data 形状对齐前端 NotifyListData（{items}），NotifyBanner 无需适配层。
	out := make([]map[string]interface{}, 0, len(items))
	for _, it := range items {
		out = append(out, map[string]interface{}{
			"id":       it.ID,
			"title":    it.Title,
			"content":  it.Content,
			"type":     it.Type,
			"level":    it.Level,
			"start_at": it.StartAt,
			"end_at":   it.EndAt,
		})
	}
	c.JSON(consts.StatusOK, map[string]interface{}{"code": 0, "message": "ok", "data": map[string]interface{}{"items": out}})
}
