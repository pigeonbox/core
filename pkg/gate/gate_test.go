package gate

import (
	"testing"

	"github.com/pigeonbox/contracts/errcode"
	"github.com/pigeonbox/core/conf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func withConf(t *testing.T, cfg *conf.AppConfiguration) {
	t.Helper()
	old := conf.GetGlobalConfig()
	conf.SetGlobalConfig(cfg)
	t.Cleanup(func() { conf.SetGlobalConfig(old) })
}

func uid(v uint) *uint { return &v }

func TestCheckUploadAllowed(t *testing.T) {
	t.Run("配置未初始化放行", func(t *testing.T) {
		withConf(t, nil)
		assert.NoError(t, CheckUploadAllowed(nil))
	})
	t.Run("开关开匿名放行", func(t *testing.T) {
		withConf(t, &conf.AppConfiguration{Upload: conf.UploadConfig{OpenUpload: true}})
		assert.NoError(t, CheckUploadAllowed(nil))
	})
	t.Run("开关关拒绝匿名", func(t *testing.T) {
		withConf(t, &conf.AppConfiguration{Upload: conf.UploadConfig{OpenUpload: false}})
		err := CheckUploadAllowed(nil)
		assert.Error(t, err)
		if ge, ok := err.(*GateError); ok {
			assert.Equal(t, 10012, ge.ErrCode())
		} else {
			t.Fatalf("want *GateError, got %T", err)
		}
	})
	t.Run("开关关不影响登录用户", func(t *testing.T) {
		withConf(t, &conf.AppConfiguration{Upload: conf.UploadConfig{OpenUpload: false}})
		assert.NoError(t, CheckUploadAllowed(uid(1)))
	})
}

func TestCheckUploadLogin(t *testing.T) {
	t.Run("要求登录时拒绝匿名", func(t *testing.T) {
		withConf(t, &conf.AppConfiguration{Upload: conf.UploadConfig{RequireLogin: true}})
		err := CheckUploadLogin(nil)
		assert.Error(t, err)
		if ge, ok := err.(*GateError); ok {
			assert.Equal(t, 10002, ge.ErrCode())
		}
	})
	t.Run("登录用户放行", func(t *testing.T) {
		withConf(t, &conf.AppConfiguration{Upload: conf.UploadConfig{RequireLogin: true}})
		assert.NoError(t, CheckUploadLogin(uid(1)))
	})
	t.Run("默认不要求", func(t *testing.T) {
		withConf(t, &conf.AppConfiguration{})
		assert.NoError(t, CheckUploadLogin(nil))
	})
}

func TestCheckDownloadLogin(t *testing.T) {
	t.Run("要求登录时拒绝匿名", func(t *testing.T) {
		withConf(t, &conf.AppConfiguration{Download: conf.DownloadConfig{RequireLogin: true}})
		assert.Error(t, CheckDownloadLogin(nil))
	})
	t.Run("默认不要求", func(t *testing.T) {
		withConf(t, nil)
		assert.NoError(t, CheckDownloadLogin(nil))
	})
}

// ---- 单用户单次上传大小上限（2026-10-19 事故：typed error 化，CodeTooLarge→413）----

func TestCheckUploadSizeCap(t *testing.T) {
	uid := uint(7)
	t.Run("登录用户超上限拒绝且携带 CodeTooLarge", func(t *testing.T) {
		err := CheckUploadSizeCap(&uid, 200, 100)
		var coder interface{ ErrCode() int }
		require.ErrorAs(t, err, &coder)
		assert.Equal(t, errcode.CodeTooLarge, coder.ErrCode())
		assert.Contains(t, err.Error(), "上传总大小超过限制")
	})
	t.Run("未超上限放行", func(t *testing.T) {
		assert.NoError(t, CheckUploadSizeCap(&uid, 100, 100))
		assert.NoError(t, CheckUploadSizeCap(&uid, 50, 100))
	})
	t.Run("上限 0 = 不限", func(t *testing.T) {
		assert.NoError(t, CheckUploadSizeCap(&uid, 1<<40, 0))
	})
	t.Run("匿名跳过（走匿名日配额另一闸口）", func(t *testing.T) {
		assert.NoError(t, CheckUploadSizeCap(nil, 1<<40, 100))
	})
}
