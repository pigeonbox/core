// 缩略图取件面（GET /preview/:code/thumb）批 1（2026-11-03）
//
// 此前 Thumbnail 落盘后无人能取：响应直出服务器文件系统路径（假 URL + 路径
// 泄漏），previews 缓存目录无服务路由 ⇒ 缩略图零消费。本面按 code 走与
// /preview/:code 同款取件闸门后回缓存文件；响应出口 renderThumbURL 统一把
// 落盘位置改写为 /preview/<code>/thumb。
//
// 断言必须能变红（变异负对照已实做）：
//   - 防穿越是**纵深三道**（filepath.Base 剥目录 + filepath.Join 清洗 + Rel 复核），
//     单撤任何一道都有另一道接住；规范化整体撤除（直拼字符串+撤 Rel+撤 Base）后
//     深穿越 fixture 真逃逸到 /etc/passwd ⇒ TestGetThumb_TraversalRejected 红；
//   - 撤 authorizePickup 的密码闸 ⇒ TestGetThumb_PasswordGate 红；
//   - 撤 renderThumbURL 改写 ⇒ TestRenderThumbURL 红。
package preview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	previewService "github.com/pigeonbox/core/preview"
	"github.com/pigeonbox/core/repo/db/model"
)

// newThumbTestEnv 构造内存库 + 注入临时缓存目录的预览引擎（GetThumb 经
// preview.GetService().CachePath() 定位缩略图，测试须注入真实单例）。
func newThumbTestEnv(t *testing.T) (*Service, *gorm.DB, string) {
	t.Helper()
	gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, gormDB.AutoMigrate(&model.FileCode{}, &model.FilePreview{}))
	sqlDB, err := gormDB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	cacheDir := t.TempDir()
	prevSvc := previewService.NewService(&previewService.Config{
		EnablePreview:    true,
		ThumbnailWidth:   300,
		ThumbnailHeight:  200,
		MaxFileSize:      50 * 1024 * 1024,
		PreviewCachePath: cacheDir,
		FFmpegPath:       "ffmpeg",
	})
	old := previewService.GetService()
	previewService.SetService(prevSvc)
	t.Cleanup(func() { previewService.SetService(old) })

	return NewService(gormDB, nil), gormDB, cacheDir
}

// TestGetThumb_PasswordGate 密码保护分享不带头 ⇒ 拒（缩略图也是「拿内容」
// 的面，闸门不全 ⇒ 缩略图成旁路提取通道）。
func TestGetThumb_PasswordGate(t *testing.T) {
	svc, gormDB, _ := newThumbTestEnv(t)
	ctx := context.Background()
	require.NoError(t, gormDB.Create(&model.FileCode{
		Code: "THUMB001", Text: "a.png", FilePath: "uploads", UUIDFileName: "a.png",
		Size: 10, ExpiredCount: 3, RequireAuth: true,
		PasswordHash: "$2a$10$fakehashfakehashfakehashfakehashfakehashfakehashfake",
	}).Error)

	_, err := svc.GetThumb(ctx, GetReq{Code: "THUMB001", ClientIP: "1.2.3.4", Password: ""})
	require.True(t, errors.Is(err, ErrPasswordRequired),
		"密码分享不带头必须拒，实得 %v", err)
}

// TestGetThumb_TraversalRejected DB 里的 Thumbnail 是穿越值（历史行=落盘
// 全路径，值虽非用户直参仍按不可信输入防守）⇒ 拒且不落任何盘外读。
func TestGetThumb_TraversalRejected(t *testing.T) {
	svc, gormDB, _ := newThumbTestEnv(t)
	ctx := context.Background()
	require.NoError(t, gormDB.Create(&model.FileCode{
		Code: "THUMB002", Text: "a.png", FilePath: "uploads", UUIDFileName: "a.png",
		Size: 10, ExpiredCount: 3,
	}).Error)
	fc, err := svc.fileCodeRepo.GetByCode(ctx, "THUMB002")
	require.NoError(t, err)
	require.NoError(t, svc.previewRepo.Create(ctx, &model.FilePreview{
		FileCodeID: fc.ID, PreviewType: "image",
		// 十级 ..：temp 目录层级深，浅穿越撞不到真实盘外文件；
		// 取 /etc/passwd（两平台皆存在）验「逃逸后能真读到盘外」的防线
		Thumbnail: "../../../../../../../../../.." + string(filepath.Separator) + "etc/passwd",
	}))

	_, err = svc.GetThumb(ctx, GetReq{Code: "THUMB002", ClientIP: "1.2.3.4"})
	require.True(t, errors.Is(err, ErrPreviewUnavailable),
		"穿越值必须拒（ErrPreviewUnavailable ⇒ 404），实得 %v", err)
}

// TestGetThumb_ReturnsFile 正常路径：缓存目录里的缩略图按 base 定位后回全路径。
// 存量行（全路径）与新形态（纯文件名）都兼容——只取 base 拼缓存目录。
func TestGetThumb_ReturnsFile(t *testing.T) {
	svc, gormDB, cacheDir := newThumbTestEnv(t)
	ctx := context.Background()
	full := filepath.Join(cacheDir, "a_thumb.png")
	require.NoError(t, os.WriteFile(full, []byte("png-bytes"), 0644))

	// 存量形态：DB 存全路径
	require.NoError(t, gormDB.Create(&model.FileCode{
		Code: "THUMB003", Text: "a.png", FilePath: "uploads", UUIDFileName: "a.png",
		Size: 10, ExpiredCount: 3,
	}).Error)
	fc, err := svc.fileCodeRepo.GetByCode(ctx, "THUMB003")
	require.NoError(t, err)
	require.NoError(t, svc.previewRepo.Create(ctx, &model.FilePreview{
		FileCodeID: fc.ID, PreviewType: "image", Thumbnail: full,
	}))
	got, err := svc.GetThumb(ctx, GetReq{Code: "THUMB003", ClientIP: "1.2.3.4"})
	require.NoError(t, err)
	require.Equal(t, full, got)

	// 新形态：DB 存纯文件名
	require.NoError(t, gormDB.Create(&model.FileCode{
		Code: "THUMB005", Text: "a.png", FilePath: "uploads", UUIDFileName: "a.png",
		Size: 10, ExpiredCount: 3,
	}).Error)
	fc5, err := svc.fileCodeRepo.GetByCode(ctx, "THUMB005")
	require.NoError(t, err)
	require.NoError(t, svc.previewRepo.Create(ctx, &model.FilePreview{
		FileCodeID: fc5.ID, PreviewType: "image", Thumbnail: "a_thumb.png",
	}))
	got, err = svc.GetThumb(ctx, GetReq{Code: "THUMB005", ClientIP: "1.2.3.4"})
	require.NoError(t, err)
	require.Equal(t, full, got)
}

// TestGetThumb_CountExhausted 次数耗尽 ⇒ 拒（share_surface 契约表 CoveredBy
// 指向本用例：缩略图是内容派生面，与 /preview/:code 同 20004 口径）。
func TestGetThumb_CountExhausted(t *testing.T) {
	svc, gormDB, _ := newThumbTestEnv(t)
	ctx := context.Background()
	require.NoError(t, gormDB.Create(&model.FileCode{
		Code: "THUMB007", Text: "a.png", FilePath: "uploads", UUIDFileName: "a.png",
		Size: 10, ExpiredCount: 0,
	}).Error)

	_, err := svc.GetThumb(ctx, GetReq{Code: "THUMB007", ClientIP: "1.2.3.4"})
	require.True(t, errors.Is(err, ErrPickupExhausted), "实得 %v", err)
}

// TestGetThumb_NoThumbnail 无缩略图/无预览记录 ⇒ ErrPreviewUnavailable（404 口径）。
func TestGetThumb_NoThumbnail(t *testing.T) {
	svc, gormDB, _ := newThumbTestEnv(t)
	ctx := context.Background()
	require.NoError(t, gormDB.Create(&model.FileCode{
		Code: "THUMB004", Text: "a.txt", FilePath: "uploads", UUIDFileName: "a.txt",
		Size: 10, ExpiredCount: 3,
	}).Error)
	fc, err := svc.fileCodeRepo.GetByCode(ctx, "THUMB004")
	require.NoError(t, err)
	require.NoError(t, svc.previewRepo.Create(ctx, &model.FilePreview{
		FileCodeID: fc.ID, PreviewType: "text", Thumbnail: "",
	}))

	_, err = svc.GetThumb(ctx, GetReq{Code: "THUMB004", ClientIP: "1.2.3.4"})
	require.True(t, errors.Is(err, ErrPreviewUnavailable), "实得 %v", err)

	// 无预览记录同样 404 口径
	require.NoError(t, gormDB.Create(&model.FileCode{
		Code: "THUMB006", Text: "a.txt", FilePath: "uploads", UUIDFileName: "a.txt",
		Size: 10, ExpiredCount: 3,
	}).Error)
	_, err = svc.GetThumb(ctx, GetReq{Code: "THUMB006", ClientIP: "1.2.3.4"})
	require.True(t, errors.Is(err, ErrPreviewUnavailable), "实得 %v", err)
}

// TestRenderThumbURL 响应出口改写：空不改、非空改 /preview/<code>/thumb。
// 三条返回路径（缓存命中/新生成）都过本函数——撤改写即红。
func TestRenderThumbURL(t *testing.T) {
	svc, _, _ := newThumbTestEnv(t)

	require.Nil(t, svc.renderThumbURL(nil, "ABC"))
	empty := &model.FilePreview{}
	require.Same(t, empty, svc.renderThumbURL(empty, "ABC"), "空 Thumbnail 不应改写")
	require.Empty(t, empty.Thumbnail)

	p := &model.FilePreview{Thumbnail: "/var/data/previews/a_thumb.png"}
	got := svc.renderThumbURL(p, "ABC")
	require.Equal(t, "/preview/ABC/thumb", got.Thumbnail, "响应面必须是可取 URL（假路径直出=路径泄漏）")
}
