// count 型分享的预览正文面（core#170）
//
// 预览面原样回传 text_content（文本/代码类文件的前 1MB ≈ 全文）但**从不扣次**
// ⇒ count 型「最多取 N 次」对预览面形同虚设：反复 GET /preview/<code> 即可
// 无限读正文（MCP preview_share 与本面同走 GetOrCreate，同源同修）。
//
// 拍板口径（计次设计稿 §3 不变量 1 的交付面枚举**原样不动** ⇒ 17 条一条不破）：
// **仅无限次（ExpiredCount=-1）分享开放正文预览**；count 型只回截断摘要 +
// 「完整内容请取件」引导，全文仍走扣次的 select/download 面。
// 不选「preview 扣次」的理由：本面没有下载令牌 ⇒ 扣次键只能 (码,IP)，与
// /share/download 的令牌指纹键分叉 ⇒ 预览+下载对同一取件人扣 2 次（D1 家族
// 回归），且 N=1 分享开页自动预览即烧掉唯一一次 ⇒ 收件人永远下不了文件。
//
// 断言必须能变红：撤掉 renderOutput 的截断 ⇒ 首个用例红；
// 把 -1 无限次哨兵误判成 count 型 ⇒ Unlimited 用例红。
package preview

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/repo/db/model"
)

const (
	countBodyHead = "HEAD-"
	countBodyTail = "TAIL-MARKER-END"
)

// longBody 构造超长正文（>500 上限），中段带尾部标记用于「不得整段回传」断言。
func longBody() string {
	return countBodyHead + strings.Repeat("A", 600) + countBodyTail + strings.Repeat("B", 1400)
}

// seedTextPreview 建分享 + 既有 text 预览行（缓存命中路径，不触碰存储）。
func seedTextPreview(t *testing.T, gormDB *gorm.DB, code string, expiredCount int, text string) {
	t.Helper()
	require.NoError(t, gormDB.Create(&model.FileCode{
		Code: code, Text: "a.txt", FilePath: "uploads", UUIDFileName: "a.txt",
		Size: int64(len(text)), ExpiredCount: expiredCount,
	}).Error)
	var fc model.FileCode
	require.NoError(t, gormDB.Where("code = ?", code).First(&fc).Error)
	require.NoError(t, gormDB.Create(&model.FilePreview{
		FileCodeID: fc.ID, PreviewType: "text", TextContent: text,
	}).Error)
}

func openPreviewBodyDB(t *testing.T) *gorm.DB {
	t.Helper()
	gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, gormDB.AutoMigrate(&model.FileCode{}, &model.FilePreview{}))
	sqlDB, err := gormDB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return gormDB
}

// TestPreview_CountLimited_TruncatesBody count 型超长正文 ⇒ 只回截断摘要 + 引导。
//
// 负对照（当前缺陷态）：现行代码把整段 text_content 原样回传 ⇒
// NotContains(countBodyTail) 与 Truncated 断言均红。
func TestPreview_CountLimited_TruncatesBody(t *testing.T) {
	gormDB := openPreviewBodyDB(t)
	seedTextPreview(t, gormDB, "LTD00001", 1, longBody())

	svc := NewService(gormDB, nil)
	out, err := svc.GetOrCreate(context.Background(), GetReq{Code: "LTD00001", ClientIP: "10.9.0.1"})
	require.NoError(t, err)
	require.True(t, out.Truncated, "count 型超长正文必须带截断标记（供前端/AI 展示引导）")
	require.Contains(t, out.TextContent, "预览截断", "截断必须带「完整内容请取件」引导")
	require.NotContains(t, out.TextContent, countBodyTail, "正文尾部不得经预览面泄漏（缺陷态：整段回传）")
	require.Less(t, utf8.RuneCountInString(out.TextContent), utf8.RuneCountInString(longBody()))
}

// TestPreview_Unlimited_KeepsFullBody 无限次（-1）⇒ 正文预览不受限（行业惯例口径）。
func TestPreview_Unlimited_KeepsFullBody(t *testing.T) {
	gormDB := openPreviewBodyDB(t)
	seedTextPreview(t, gormDB, "UNL00001", -1, longBody())

	svc := NewService(gormDB, nil)
	out, err := svc.GetOrCreate(context.Background(), GetReq{Code: "UNL00001", ClientIP: "10.9.0.2"})
	require.NoError(t, err)
	require.Equal(t, longBody(), out.TextContent, "无限次分享必须回完整正文")
	require.False(t, out.Truncated, "无限次分享不应带截断标记")
}

// TestPreview_CountLimited_ShortBodyNotTruncated count 型短正文 ⇒ 摘要尺寸内不截、不加标记。
func TestPreview_CountLimited_ShortBodyNotTruncated(t *testing.T) {
	gormDB := openPreviewBodyDB(t)
	short := "short summary body"
	seedTextPreview(t, gormDB, "SHT00001", 3, short)

	svc := NewService(gormDB, nil)
	out, err := svc.GetOrCreate(context.Background(), GetReq{Code: "SHT00001", ClientIP: "10.9.0.3"})
	require.NoError(t, err)
	require.Equal(t, short, out.TextContent, "上限内的正文原样回传（超长才截）")
	require.False(t, out.Truncated)
}
