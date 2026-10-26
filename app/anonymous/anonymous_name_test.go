package anonymous

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/repo/db/model"
)

func newAnonTestService(t *testing.T) *Service {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}, &model.FileCodeFile{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return NewService(nil, g)
}

// TestBindFilePath_KeepsOriginalFileName 回归（2026-10-26 审计）：
// 匿名取件码通道的**原始文件名曾被永久丢弃**——占位记录把原始名寄存在
// `uuid_file_name`（占位期 file_path 为空 ⇒ 不能写 text，否则 IsTextShare() 会把
// 「待上传的文件分享」误判成文本分享），而绑定时又把 uuid_file_name 清空 ⇒
// 原始名消失 ⇒ 展示名退化成存储名（up_<uuid>.pdf），取件页/管理台/用户列表/
// MCP/传输日志全部显示 UUID。
// ⇒ 绑定时把原始名转写进 text；uuid_file_name 回归本义（存储名）。
func TestBindFilePath_KeepsOriginalFileName(t *testing.T) {
	svc := newAnonTestService(t)
	ctx := context.Background()

	// 占位记录：原始名寄存在 uuid_file_name（与 generate 通道同形态）
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
		Code:         "ANON01",
		UploadType:   "anonymous",
		UUIDFileName: "季度报告.pdf",
		ExpiredAt:    ptrTime(time.Now().Add(time.Hour)),
		ExpiredCount: -1,
	}))

	// 直传完成 → 回填
	require.NoError(t, svc.BindFilePath(ctx, "ANON01", "uploads/2026/10/26/up_a1b2c3.pdf", 123, ""))

	fc, err := svc.fileCodeRepo.GetByCode(ctx, "ANON01")
	require.NoError(t, err)

	assert.Equal(t, "季度报告.pdf", fc.Text, "原始文件名必须保留（不能退化成存储 UUID）")
	assert.Equal(t, "up_a1b2c3.pdf", fc.UUIDFileName, "uuid_file_name 回归本义=存储名")
	assert.Equal(t, "uploads/2026/10/26/up_a1b2c3.pdf", fc.GetFilePath(), "file_path 是完整相对路径")
	assert.Equal(t, "季度报告.pdf", fc.DisplayName(), "展示名 = 原始文件名")
	assert.False(t, fc.IsTextShare(), "已绑定文件的分享不得被误判为文本分享")
}

func ptrTime(t time.Time) *time.Time { return &t }
