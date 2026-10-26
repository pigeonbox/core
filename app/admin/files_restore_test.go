// 「删除 → 回收站 → 恢复」必须恢复**子表行**（2026-10-26 审计 P1）。
//
// 缺陷：v0.32.14 起 file_code_files 子表是文件清单的**唯一真相源**（虚拟行退役）。
// 删除侧（BatchDeleteFiles）会软删子行，恢复侧（RestoreByIDs）此前只 UPDATE
// 主表 deleted_at ⇒ 子行仍软删 ⇒ 恢复后：取件页清单为空 / zip 无内容 /
// 逐文件下载 404 ⇒ 「恢复」只恢复了一半。
//
// 断言必须能变红（负对照）：把 dao.RestoreByIDs 里那段「恢复子行」的 UPDATE
// 去掉，本用例即失败（子表查不到行 / ListShareFiles 为空）。
package admin

import (
	"context"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	shareApp "github.com/pigeonbox/core/app/share"
	"github.com/pigeonbox/core/repo/db/model"
)

// newRestoreTestDB 独立内存库（钉单连接：glebarez/sqlite 的 :memory: 每连接
// 是独立库，多连接会让数据「凭空消失」）。
func newRestoreTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}, &model.FileCodeFile{},
		&model.SystemConfigRecord{}, &model.AdminOperationLog{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return g
}

// TestRestoreFiles_RestoresChildRows 恢复分享时子表行必须一并复活。
func TestRestoreFiles_RestoresChildRows(t *testing.T) {
	g := newRestoreTestDB(t)
	svc := NewService(g)
	svc.SetStorage(&mockStorage{}) // 有存储 ⇒ 删除侧才会软删子行（真实链路同此）
	ctx := context.Background()

	// 主表 1 行 + 子表 2 行（多文件分享）
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
		Code: "REST01", FilePath: "uploads/2026/10/26/a.txt", Text: "a.txt",
		Size: 11, ExpiredCount: -1,
	}))
	fc, err := svc.fileCodeRepo.GetByCode(ctx, "REST01")
	require.NoError(t, err)
	require.NoError(t, svc.fileFileRepo.CreateBatch(ctx, []*model.FileCodeFile{
		{FileCodeID: fc.ID, FileName: "a.txt", UUIDFileName: "a.txt", FilePath: "uploads/2026/10/26/a.txt", Size: 11, SortOrder: 0},
		{FileCodeID: fc.ID, FileName: "b.txt", UUIDFileName: "b.txt", FilePath: "uploads/2026/10/26/b.txt", Size: 22, SortOrder: 1},
	}))

	// 删除（软删主表 + 软删子行）
	n, err := svc.BatchDeleteFiles(ctx, []uint{fc.ID})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	// 前置断言：软删后子表（默认作用域）查不到行
	children, err := svc.fileFileRepo.ListByFileCodeID(ctx, fc.ID)
	require.NoError(t, err)
	require.Empty(t, children, "删除后子行应被软删（前置条件）")

	// 恢复
	restored, err := svc.RestoreFiles(ctx, []uint{fc.ID})
	require.NoError(t, err)
	require.Equal(t, int64(1), restored)

	// ① 子表行复活（2 行）
	children, err = svc.fileFileRepo.ListByFileCodeID(ctx, fc.ID)
	require.NoError(t, err)
	assert.Len(t, children, 2, "恢复必须一并恢复子表行（子表=文件清单唯一真相源）")

	// ② 管理台详情面读到 2 个子文件
	_, detail, err := svc.GetFileDetail(ctx, fc.ID)
	require.NoError(t, err)
	assert.Len(t, detail, 2, "恢复后管理台详情必须看到完整文件清单")

	// ③ 取件端清单非空（share 域读的是同一张子表）
	shareSvc := shareApp.NewService(g, "http://localhost:12345", nil)
	items, err := shareSvc.ListShareFiles(ctx, "REST01")
	require.NoError(t, err)
	assert.Len(t, items, 2, "恢复后取件页文件清单不得为空")
}
