// count 型分享的预览正文交付（core#170）
//
// 缺陷：/preview/:code 对 PreviewTypeCode/Text 回传 text_content（CodeGenerator
// 读满 1MB 原文），而 authorizePickup 全程不扣次 ⇒ owner 设「最多取 1 次」的
// 文本/代码分享，拿到码的人反复预览即可无限读取正文，N 次限制对该面形同虚设
// （与取件计次设计稿不变量 1「扣次点 = 内容交付前」直接冲突——预览是**未扣次的
// 正文交付面**）。
//
// 口径（与 17 条不变量一致的方案）：count 型分享的预览**不交付正文**（回固定
// 引导文案，元数据照常）；全文只走已扣次的交付面（文本 select / 文件 download）。
// 无限次分享（ExpiredCount=-1）无次数语义可破 ⇒ 全文预览保持不变。
//
// 为什么不选「预览也扣次」：预览请求不携带下载令牌 ⇒ 计次键无法与下载会话共享；
// 若按 TextViewKey(code,IP) 扣，文件分享的同一取件人会被扣两次（预览一次 +
// 下载令牌一次），count=1 的收件人「先预览后下载」会被 20004 挡死——预览烧掉
// 唯一一次取件，违反「一次取件 = 一次内容交付」的取件会话直觉。
//
// 断言必须能变红：抽掉 service.go 的 applyCountBodyPolicy ⇒ 本用例红。
package preview

import (
	"context"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/repo/db/model"
)

// TestPreview_CountShare_TextBodyNotDelivered count 型分享预览不得回传正文。
func TestPreview_CountShare_TextBodyNotDelivered(t *testing.T) {
	gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, gormDB.AutoMigrate(&model.FileCode{}, &model.FilePreview{}))
	sqlDB, err := gormDB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	ctx := context.Background()
	const head = "SECRET-HEAD-MARKER"
	const tail = "SECRET-TAIL-MARKER"
	body := head + strings.Repeat("x", 2000) + tail

	// count 型（最多取 1 次）+ 已缓存的 code 预览行（缓存命中路径，不触存储）
	fc := &model.FileCode{
		Code: "CBODY01", Text: "a.go", FilePath: "uploads", UUIDFileName: "a.go",
		Size: 4096, ExpiredCount: 1,
	}
	require.NoError(t, gormDB.Create(fc).Error)
	require.NoError(t, gormDB.Create(&model.FilePreview{
		FileCodeID: fc.ID, PreviewType: "code", TextContent: body,
		MimeType: "text/plain", FileSize: 4096,
	}).Error)

	svc := NewService(gormDB, nil)
	got, err := svc.GetOrCreate(ctx, GetReq{Code: "CBODY01", ClientIP: "1.2.3.4"})
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotContains(t, got.TextContent, head,
		"count 型预览不得回传正文头部（正文交付面只有已扣次的 select/download）")
	require.NotContains(t, got.TextContent, tail,
		"count 型预览不得回传正文尾部（负对照：抽掉策略即红）")
	require.Contains(t, got.TextContent, "取件",
		"应给出「完整内容请取件」引导，而不是静默空白")
	// 元数据面不受影响（预览类型/大小仍可回显）
	require.Equal(t, "code", got.PreviewType)
	require.EqualValues(t, 4096, got.FileSize)

	// DB 缓存行必须保留原文（策略只作用于响应面，不得改写缓存）
	var row model.FilePreview
	require.NoError(t, gormDB.First(&row, "file_code_id = ?", fc.ID).Error)
	require.Contains(t, row.TextContent, tail,
		"缓存行必须保留全文（响应面截断不得持久化）")

	// 负对照：无限次分享（ExpiredCount=-1）全文预览不受影响
	fc2 := &model.FileCode{
		Code: "CBODY02", Text: "b.go", FilePath: "uploads", UUIDFileName: "b.go",
		Size: 4096, ExpiredCount: -1,
	}
	require.NoError(t, gormDB.Create(fc2).Error)
	require.NoError(t, gormDB.Create(&model.FilePreview{
		FileCodeID: fc2.ID, PreviewType: "code", TextContent: body,
		MimeType: "text/plain", FileSize: 4096,
	}).Error)
	got2, err := svc.GetOrCreate(ctx, GetReq{Code: "CBODY02", ClientIP: "1.2.3.4"})
	require.NoError(t, err)
	require.NotNil(t, got2)
	require.Contains(t, got2.TextContent, tail,
		"无限次分享必须保持全文预览（策略不是「一律截断」）")
}
