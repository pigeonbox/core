package config

import (
	"context"
	"testing"

	"github.com/pigeonbox/core/conf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 管理端配置的**字段级合并**语义（2026-10-23 真机实测后修）。
//
// 缺陷实录：PUT /admin/config 此前只做「段级」nil 保留，**段内**未提及的字段被
// 整体清零（JSON 反序列化进结构体 ⇒ 未提及字段取零值）。真机实测：
//
//	PUT {"upload_ex":{"max_file_size":1024}}
//	  ⇒ upload_ex 其余字段全部归零 ⇒ open_upload=false（匿名上传被"关闭"，
//	    /share/file/ 立刻 10012）、enable_chunk=false、enable_magic_check=false、
//	    text_max_bytes=0、local_import.roots 清空……
//	  ⇒ 看起来只改了一个字段，实际把整段配置弄没了（静默失效族）。
//
// 新语义：提交里**出现**的字段生效（显式 null 视为清空），**未出现**的字段保留
// 现状；未提交的段整体保留；嵌套对象递归合并。
func TestUpdateConfigJSON_FieldLevelMerge(t *testing.T) {
	svc := newTestConfigService(t)
	ctx := context.Background()

	// 先落一份完整配置（模拟站点已有设置）
	require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
		UploadEx: &conf.UploadConfig{
			OpenUpload:       true,
			UploadSize:       10 * 1024 * 1024,
			EnableChunk:      true,
			ChunkSize:        2 * 1024 * 1024,
			EnableMagicCheck: true,
			TextMaxBytes:     222 * 1024,
		},
		LocalImport: &conf.LocalImportConfig{
			Enabled: true,
			Roots:   []string{"/import"},
		},
	}))

	t.Run("只提交一个字段 ⇒ 同段其余字段保留现状", func(t *testing.T) {
		require.NoError(t, svc.UpdateConfigJSON(ctx, []byte(`{"upload_ex":{"max_file_size":1024}}`)))

		got, err := svc.GetConfig(ctx)
		require.NoError(t, err)
		require.NotNil(t, got.UploadEx)
		assert.Equal(t, int64(1024), got.UploadEx.MaxFileSize, "提交的字段应生效")
		// 关键断言：同段其余字段**不得**被清零
		assert.True(t, got.UploadEx.OpenUpload, "open_upload 被静默清零 ⇒ 匿名上传被关掉")
		assert.Equal(t, int64(10*1024*1024), got.UploadEx.UploadSize)
		assert.True(t, got.UploadEx.EnableChunk, "enable_chunk 被静默清零 ⇒ 分块上传被关掉")
		assert.Equal(t, int64(2*1024*1024), got.UploadEx.ChunkSize)
		assert.True(t, got.UploadEx.EnableMagicCheck, "魔数校验被静默清零")
		assert.Equal(t, int64(222*1024), got.UploadEx.TextMaxBytes)
	})

	t.Run("未提交的段整体保留", func(t *testing.T) {
		got, err := svc.GetConfig(ctx)
		require.NoError(t, err)
		require.NotNil(t, got.LocalImport)
		assert.True(t, got.LocalImport.Enabled)
		assert.Equal(t, []string{"/import"}, got.LocalImport.Roots)
	})

	t.Run("显式 null 清空该字段", func(t *testing.T) {
		require.NoError(t, svc.UpdateConfigJSON(ctx, []byte(`{"upload_ex":{"allowed_expire_styles":null}}`)))
		got, err := svc.GetConfig(ctx)
		require.NoError(t, err)
		require.NotNil(t, got.UploadEx)
		assert.Nil(t, got.UploadEx.AllowedExpireStyles, "显式 null 应清空该字段")
		// 清一个字段不得牵连同段其他字段
		assert.True(t, got.UploadEx.OpenUpload)
		assert.Equal(t, int64(1024), got.UploadEx.MaxFileSize)
	})

	t.Run("嵌套对象递归合并（改 smtp.host 不清空 port/username）", func(t *testing.T) {
		require.NoError(t, svc.UpdateConfig(ctx, &SystemConfig{
			Notify: &conf.NotifyConfig{
				SMTP: conf.SMTPConfig{Host: "smtp.old", Port: 25, Username: "u1"},
			},
		}))
		require.NoError(t, svc.UpdateConfigJSON(ctx, []byte(`{"notify":{"smtp":{"host":"smtp.new"}}}`)))

		got, err := svc.GetConfig(ctx)
		require.NoError(t, err)
		require.NotNil(t, got.Notify)
		require.NotNil(t, got.Notify.SMTP)
		assert.Equal(t, "smtp.new", got.Notify.SMTP.Host)
		assert.Equal(t, 25, got.Notify.SMTP.Port, "同级 port 被清零 ⇒ 递归合并失效")
		assert.Equal(t, "u1", got.Notify.SMTP.Username, "同级 username 被清零 ⇒ 递归合并失效")
	})

	t.Run("非法体报错而不是静默保存", func(t *testing.T) {
		require.Error(t, svc.UpdateConfigJSON(ctx, []byte(`not-json`)))
		require.Error(t, svc.UpdateConfigJSON(ctx, []byte(`[1,2,3]`)))
	})
}
