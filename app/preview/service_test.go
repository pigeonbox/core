package preview

import (
	"context"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	previewService "github.com/pigeonbox/core/preview"
	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm"
)

// TestGeneratePreview_SizeGate 预览大小上限生效（2026-10-08 加固）：
// Config.MaxFileSize 此前从未被检查，任意大文件全量落临时盘 = 磁盘耗尽 DoS 面。
// storageSvc 注入 nil 即可证明超限路径在触碰存储前就被拒绝。
func TestGeneratePreview_SizeGate(t *testing.T) {
	tmp := t.TempDir()
	if err := previewService.InitService(&previewService.Config{
		EnablePreview:    true,
		MaxFileSize:      100,
		PreviewCachePath: tmp,
	}); err != nil {
		t.Fatalf("init preview service: %v", err)
	}

	svc := NewService(newPreviewTestDB(t), nil)
	ctx := context.Background()

	// DB 记录大小超限 → 拒绝且不触碰存储（nil storage 未 panic 即为证）
	big := &model.FileCode{FilePath: "uploads/2026/10/08/x.txt", Size: 200}
	if _, err := svc.generatePreview(ctx, big); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("超限文件应被大小门拒绝，got err=%v", err)
	}

	// 记录大小为 0（老数据）时放行进入流式路径 → 走到 storage nil 检查
	unknown := &model.FileCode{FilePath: "uploads/2026/10/08/y.txt", Size: 0}
	if _, err := svc.generatePreview(ctx, unknown); err == nil || !strings.Contains(err.Error(), "storage not available") {
		t.Fatalf("未知大小文件应通过预判进入流式截断路径，got err=%v", err)
	}
}

// newPreviewTestDB 构造注入用内存 sqlite（测试不触 DB 时也保持构造合法）。
func newPreviewTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	sqlDB, err := gormDB.DB()
	if err != nil {
		t.Fatalf("get sql db: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	return gormDB
}
