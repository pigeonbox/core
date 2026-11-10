// core#111 行为判据：notify List 的手写偏移 (req.Page-1)*req.PageSize 无溢出
// 保护——NotifyPolicy 只钳页长（≤100）、**不封顶页码** ⇒ 超大页码乘法回绕
// （可回绕成正数）⇒ 偏移量完全错误：page=922337203685477582、pageSize=20
// 时 (page-1)*20 = 2^64+4 ⇒ Go int 回绕成 +4 ⇒ 「超大页码」静默翻到
// 跳过最新 4 条的错误页（管理员翻通知列表翻到别人页）。
//
// 收编 paging.NotifyPolicy.Offset（乘法前溢出判定 ⇒ 溢出回退第一页）后，
// 超大页码拿到的是第一页全量。
//
// 负对照（已实跑）：把 notify.go 的 Offset(paging.NotifyPolicy.Offset(...))
// 换回 (req.Page - 1) * req.PageSize ⇒ 本用例转红（只回 2 条、最新一条被跳过）。
package notify

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/model"
)

// wrapPage 使 (page-1)*20 在 64 位 int 上回绕成 +4 的页码：
// (922337203685477582-1)*20 = 2^64+4 ⇒ 回绕 +4（跳过最新 4 条）。
const wrapPage = 922337203685477582

func TestList_HugePageOffsetNoWrap(t *testing.T) {
	g := newTestDB(t)
	svc := NewService(g)
	ctx := context.Background()

	ids := make([]uint, 0, 6)
	for i := 0; i < 6; i++ {
		item, err := svc.Create(ctx, CreateReq{
			Title: "t", Content: "c", Type: "maintenance", Level: "info", AuthorID: 1,
		})
		require.NoError(t, err)
		ids = append(ids, item.ID)
	}
	// created_at 逐条拉开（DESC 次序钉死，最新 = 最后创建那条）
	for i, id := range ids {
		ts := time.Now().Add(-time.Duration(10-i) * time.Hour)
		require.NoError(t, g.Model(&model.Notify{}).Where("id = ?", id).Update("created_at", ts).Error)
	}
	newest := ids[len(ids)-1]

	// 文档化旧手写乘法的回绕结果（证明判据打的正是溢出面）。
	// 用变量做运行期乘法（常量表达式在编译期就溢出报错）。
	p, sz := wrapPage, 20
	if wrapped := (p - 1) * sz; wrapped != 4 {
		t.Fatalf("测试常量失效：(wrapPage-1)*20 = %d，want 4", wrapped)
	}

	data, err := svc.List(ctx, ListReq{Page: wrapPage, PageSize: 20})
	require.NoError(t, err, "超大页码不得打穿 SQL")
	assert.Len(t, data.Items, 6,
		"超大页码溢出应回退第一页全量（修前回绕偏移 +4 ⇒ 只回 2 条、最新被跳过 ⇒ 翻到别人页）")
	require.NotEmpty(t, data.Items)
	assert.Equal(t, newest, data.Items[0].ID, "第一页必须从最新一条开始（偏移不得回绕）")

	// 正常页码行为不动（同域归一化口径零变化）
	page2, err := svc.List(ctx, ListReq{Page: 2, PageSize: 2})
	require.NoError(t, err)
	assert.Len(t, page2.Items, 2, "正常分页条数不动（口径一字不改）")
	assert.Equal(t, ids[3], page2.Items[0].ID, "第 2 页从第 5 新开始（offset=2）")
}
