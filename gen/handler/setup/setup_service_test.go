package setup

import (
	"testing"

	setupmodel "github.com/pigeonbox/contracts/gen/setup"
	"github.com/pigeonbox/core/conf"
	"github.com/stretchr/testify/assert"
)

// TestApplyUploadPreConfig_MaxFileSize 钉住「单文件上限」的映射语义
// （2026-10-23）。
//
// 背景：首启向导此前唯一的体积项 upload_size_mb 被标注成「单文件上限」，
// 实际写的是单请求体上限；管理员据此以为限住了单文件大小，实际大文件照传
// （2026-10-23 的 413 事故之源）。真上限由 max_file_size_mb →
// upload.max_file_size 承载，语义是 optional 三态：不传=保持底稿、
// 传 0=不限、传正数=该值。
func TestApplyUploadPreConfig_MaxFileSize(t *testing.T) {
	base := conf.UploadConfig{
		OpenUpload:   true,
		UploadSize:   10 * 1024 * 1024,
		MaxFileSize:  12345, // 底稿里的既有值
		ChunkSize:    2 * 1024 * 1024,
		EnableChunk:  true,
		TextMaxBytes: 222 * 1024,
	}

	t.Run("不传 = 保持底稿", func(t *testing.T) {
		sc := &setupmodel.SitePreConfig{UploadSizeMb: 100, OpenUpload: true}
		out := applyUploadPreConfig(base, sc)
		assert.Equal(t, int64(12345), out.MaxFileSize, "未携带 max_file_size_mb 时应保持底稿")
	})

	t.Run("传 0 = 显式不限", func(t *testing.T) {
		zero := int32(0)
		sc := &setupmodel.SitePreConfig{UploadSizeMb: 100, OpenUpload: true}
		sc.MaxFileSizeMb = &zero
		out := applyUploadPreConfig(base, sc)
		assert.Equal(t, int64(0), out.MaxFileSize, "显式传 0 应表示不限")
	})

	t.Run("传正值 = 按 MB 换算成字节", func(t *testing.T) {
		mb := int32(4096)
		sc := &setupmodel.SitePreConfig{UploadSizeMb: 100, OpenUpload: true}
		sc.MaxFileSizeMb = &mb
		out := applyUploadPreConfig(base, sc)
		assert.Equal(t, int64(4096)*1024*1024, out.MaxFileSize)
	})
}

// TestApplyUploadPreConfig_UploadSizeIsRequestBodyLimit 钉住两个上限的语义分离：
// upload_size_mb → upload.upload_size（单次请求体），max_file_size_mb →
// upload.max_file_size（整文件）。二者不可互相顶替。
func TestApplyUploadPreConfig_UploadSizeIsRequestBodyLimit(t *testing.T) {
	base := conf.UploadConfig{
		OpenUpload:   true,
		UploadSize:   10 * 1024 * 1024,
		TextMaxBytes: 222 * 1024,
	}
	mb := int32(50)
	sc := &setupmodel.SitePreConfig{UploadSizeMb: 100, OpenUpload: false}
	sc.MaxFileSizeMb = &mb

	out := applyUploadPreConfig(base, sc)

	// 单请求体上限 = 100MB
	assert.Equal(t, int64(100)*1024*1024, out.UploadSize)
	// 单文件上限 = 50MB，且与前者独立
	assert.Equal(t, int64(50)*1024*1024, out.MaxFileSize)
	assert.False(t, out.OpenUpload)
	// 其余上传项不被清零（段级整体替换语义：缺底稿会清成零值）
	assert.Equal(t, int64(222*1024), out.TextMaxBytes)
}
