package gate

import (
	"strings"
	"testing"
	"time"

	"github.com/pigeonbox/contracts/errcode"
	"github.com/pigeonbox/core/conf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- 单文件上限（upload.max_file_size）----

func TestCheckFileSize(t *testing.T) {
	t.Run("超限返回typed error且文案带上限值", func(t *testing.T) {
		withConf(t, &conf.AppConfiguration{Upload: conf.UploadConfig{MaxFileSize: 1024}})
		err := CheckFileSize(1025)
		require.Error(t, err)
		var coder interface{ ErrCode() int }
		require.ErrorAs(t, err, &coder)
		assert.Equal(t, errcode.CodeTooLarge, coder.ErrCode())
		assert.Equal(t, "文件过大: 最大允许 1024 字节", err.Error())
	})
	t.Run("未超限放行", func(t *testing.T) {
		withConf(t, &conf.AppConfiguration{Upload: conf.UploadConfig{MaxFileSize: 1024}})
		require.NoError(t, CheckFileSize(1024))
		require.NoError(t, CheckFileSize(1))
	})
	t.Run("0=不限恒放行", func(t *testing.T) {
		withConf(t, &conf.AppConfiguration{Upload: conf.UploadConfig{MaxFileSize: 0}})
		require.NoError(t, CheckFileSize(1<<40))
	})
	t.Run("配置未初始化=不限", func(t *testing.T) {
		withConf(t, nil)
		require.NoError(t, CheckFileSize(1<<40))
		assert.Equal(t, int64(0), MaxFileSize())
	})
	t.Run("非正数大小放行（入参校验归调用点）", func(t *testing.T) {
		withConf(t, &conf.AppConfiguration{Upload: conf.UploadConfig{MaxFileSize: 1024}})
		require.NoError(t, CheckFileSize(0))
		require.NoError(t, CheckFileSize(-1))
	})
}

func TestExceedsFileSize(t *testing.T) {
	withConf(t, &conf.AppConfiguration{Upload: conf.UploadConfig{MaxFileSize: 2048}})
	exceeded, limit := ExceedsFileSize(2049)
	assert.True(t, exceeded)
	assert.Equal(t, int64(2048), limit)
	exceeded, limit = ExceedsFileSize(2048)
	assert.False(t, exceeded)
	assert.Equal(t, int64(2048), limit)

	// 0=不限：恒不超限，limit 回 0（调用方不拿它拼文案）
	withConf(t, &conf.AppConfiguration{Upload: conf.UploadConfig{MaxFileSize: 0}})
	exceeded, limit = ExceedsFileSize(1 << 40)
	assert.False(t, exceeded)
	assert.Equal(t, int64(0), limit)
}

// ---- 单请求体上限（upload.upload_size）----

func TestExceedsRequestSize(t *testing.T) {
	withConf(t, &conf.AppConfiguration{Upload: conf.UploadConfig{UploadSize: 100}})
	exceeded, limit := ExceedsRequestSize(101)
	assert.True(t, exceeded)
	assert.Equal(t, int64(100), limit)
	exceeded, _ = ExceedsRequestSize(100)
	assert.False(t, exceeded)

	withConf(t, &conf.AppConfiguration{Upload: conf.UploadConfig{UploadSize: 0}})
	exceeded, limit = ExceedsRequestSize(1 << 40)
	assert.False(t, exceeded)
	assert.Equal(t, int64(0), limit)
}

// ---- 文本分享上限（upload.text_max_bytes）----

func TestExceedsTextShare(t *testing.T) {
	t.Run("未配置回退默认222KB", func(t *testing.T) {
		withConf(t, &conf.AppConfiguration{Upload: conf.UploadConfig{TextMaxBytes: 0}})
		assert.Equal(t, int64(222*1024), TextShareMaxBytes())
		exceeded, limit := ExceedsTextShare(222*1024 + 1)
		assert.True(t, exceeded)
		assert.Equal(t, int64(222*1024), limit)
	})
	t.Run("配置生效", func(t *testing.T) {
		withConf(t, &conf.AppConfiguration{Upload: conf.UploadConfig{TextMaxBytes: 10}})
		exceeded, limit := ExceedsTextShare(11)
		assert.True(t, exceeded)
		assert.Equal(t, int64(10), limit)
		exceeded, _ = ExceedsTextShare(10)
		assert.False(t, exceeded)
	})
}

// ---- 应用层请求体预算 ----

func TestHTTPBodyLimit(t *testing.T) {
	t.Run("取两闸较大值", func(t *testing.T) {
		withConf(t, &conf.AppConfiguration{Upload: conf.UploadConfig{
			UploadSize:  10 << 20,
			MaxFileSize: 4 << 20,
		}})
		assert.Equal(t, int64(10<<20), HTTPBodyLimit())
	})
	t.Run("整文件上限更大时抬高请求体上限", func(t *testing.T) {
		withConf(t, &conf.AppConfiguration{Upload: conf.UploadConfig{
			UploadSize:  10 << 20,
			MaxFileSize: 100 << 20,
		}})
		assert.Equal(t, int64(100<<20), HTTPBodyLimit())
	})
	t.Run("upload_size未配置回退10MB", func(t *testing.T) {
		withConf(t, &conf.AppConfiguration{Upload: conf.UploadConfig{UploadSize: 0, MaxFileSize: 0}})
		assert.Equal(t, int64(10<<20), HTTPBodyLimit())
	})
	t.Run("配置未初始化回退10MB", func(t *testing.T) {
		withConf(t, nil)
		assert.Equal(t, int64(10<<20), HTTPBodyLimit())
	})
}

// ---- 后台任务预算 ----

func TestCheckTaskBudget(t *testing.T) {
	t.Run("合规预算通过", func(t *testing.T) {
		require.NoError(t, CheckTaskBudget("maintenance-janitor", 24*time.Hour, 10*time.Minute))
		require.NoError(t, CheckTaskBudget("gc-jobs-worker", time.Minute, 0))
		require.NoError(t, CheckTaskBudget("expired-file-cleanup", time.Hour, time.Hour))
	})
	t.Run("无名任务拒绝", func(t *testing.T) {
		err := CheckTaskBudget("", time.Hour, 0)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "名称")
	})
	t.Run("非正周期拒绝", func(t *testing.T) {
		require.Error(t, CheckTaskBudget("x", 0, 0))
		require.Error(t, CheckTaskBudget("x", -time.Second, 0))
	})
	t.Run("低于地板拒绝", func(t *testing.T) {
		err := CheckTaskBudget("x", time.Second, 0)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "预算地板")
	})
	t.Run("负首跑延迟拒绝", func(t *testing.T) {
		require.Error(t, CheckTaskBudget("x", time.Hour, -time.Second))
	})
	t.Run("首跑延迟超过周期拒绝", func(t *testing.T) {
		require.Error(t, CheckTaskBudget("x", time.Hour, 2*time.Hour))
	})
	t.Run("地板同源：与联邦同步周期下限一致", func(t *testing.T) {
		assert.Equal(t, time.Minute, MinTaskInterval)
	})
}

// TestResourceErrorsCarryTooLargeCode 执行点被掏空的兜底：标准形态必须始终
// 携带 10010（前端动态文案白名单 + HTTP 413 的来源），文案必须带上限值。
func TestResourceErrorsCarryTooLargeCode(t *testing.T) {
	withConf(t, &conf.AppConfiguration{Upload: conf.UploadConfig{MaxFileSize: 1}})
	err := CheckFileSize(2)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "最大允许 1 字节"))
	var coder interface{ ErrCode() int }
	require.ErrorAs(t, err, &coder)
	assert.Equal(t, 10010, coder.ErrCode())
}
