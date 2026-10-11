package share

import (
	"context"
	"testing"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 大小写折叠查询：分享码/自定义口令的 DB 兜底（默认开，可关）。

// setCodeFold 换全局配置的折叠开关（与 globalquota_test 同一换配置模式）。
func setCodeFold(t *testing.T, on bool) {
	t.Helper()
	old := conf.GetGlobalConfig()
	var cfg conf.AppConfiguration
	if old != nil {
		cfg = *old
	}
	cfg.Download.CodeCaseInsensitive = &on
	conf.SetGlobalConfig(&cfg)
	t.Cleanup(func() { conf.SetGlobalConfig(old) })
}

func TestGetFileByCode_CaseFold(t *testing.T) {
	svc, gormDB, _, _, _ := newTestServiceWithDB(t)
	ctx := context.Background()

	require.NoError(t, gormDB.Create(&model.FileCode{
		Code: "AbCd1234", Status: model.StatusNormal, ExpiredCount: -1,
	}).Error)

	t.Run("折叠开：小写输入命中", func(t *testing.T) {
		setCodeFold(t, true)
		fc, err := svc.GetFileByCode(ctx, "abcd1234")
		require.NoError(t, err)
		assert.Equal(t, "AbCd1234", fc.Code)
	})
	t.Run("折叠关：仅精确匹配", func(t *testing.T) {
		setCodeFold(t, false)
		_, err := svc.GetFileByCode(ctx, "abcd1234")
		assert.Error(t, err)
	})
}

func TestCreateWithCode_CaseFoldUniqueness(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()

	build := func(code string) *model.FileCode {
		return &model.FileCode{Code: code, Status: model.StatusNormal}
	}

	t.Run("折叠开：仅大小写不同的自定义码拒绝", func(t *testing.T) {
		setCodeFold(t, true)
		_, err := svc.createWithCode(ctx, "MyCode", build)
		require.NoError(t, err)
		_, err = svc.createWithCode(ctx, "mycode", build)
		assert.ErrorContains(t, err, "已被占用")
	})
	t.Run("折叠关：仅大小写不同允许并存", func(t *testing.T) {
		setCodeFold(t, false)
		_, err := svc.createWithCode(ctx, "Other1", build)
		require.NoError(t, err)
		_, err = svc.createWithCode(ctx, "other1", build)
		assert.NoError(t, err)
	})
}
