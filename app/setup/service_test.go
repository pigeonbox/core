// core#119 同族收口：/setup 初始化建号的 AdminEmail 此前零格式校验直入库——
// 该值随后流入 SMTP To 头（通知链路）。收口走注册面同款 utils.ValidateEmail：
// 非空非法 ⇒ typed 参数错误（400 + 10001 全站信封，点名 admin_email）；
// 空邮箱保持 A1 放行口径（建号允许不填）。
//
// 负对照（已实跑）：删掉 InitializeSystem 的 ValidateEmail 闸 ⇒ 转红
// （注入邮箱被建号落库）。
package setup

import (
	"context"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	pkgerrors "github.com/pigeonbox/core/pkg/errors"
	"github.com/pigeonbox/core/repo/db/model"
)

func newSetupService(t *testing.T) *Service {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.User{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1) // :memory: 每连接独立库，钉单连接防 flake
	return NewService(g)
}

func TestInitializeSystem_AdminEmailValidated(t *testing.T) {
	t.Run("注入类/非法 ⇒ typed 参数错误且不建号", func(t *testing.T) {
		for _, email := range []string{
			"x@y.com\r\nBcc:evil@evil.com", // CRLF 注入（SMTP To 头注入面）
			"Name <x@y.com>",               // display-name 形态
			"not-an-email",
		} {
			svc := newSetupService(t)
			err := svc.InitializeSystem(context.Background(), &InitializeSystemReq{
				AdminUsername: "admin1", AdminPassword: "secret123!", AdminEmail: email,
			})
			require.Errorf(t, err, "%q 应拒绝", email)
			var biz *pkgerrors.AppError
			require.ErrorAsf(t, err, &biz, "%q 应是 typed 参数错误", email)
			assert.Equal(t, 10001, biz.BizCode, "业务码应 10001（参数错误）")
			assert.True(t, strings.Contains(err.Error(), "admin_email"),
				"消息应点名 admin_email，实得 %q", err.Error())

			ok, ierr := svc.IsSystemInitialized(context.Background())
			require.NoError(t, ierr)
			assert.False(t, ok, "非法邮箱不得建号落库")
		}
	})

	t.Run("合法 ⇒ 建号成功", func(t *testing.T) {
		svc := newSetupService(t)
		require.NoError(t, svc.InitializeSystem(context.Background(), &InitializeSystemReq{
			AdminUsername: "admin1", AdminPassword: "secret123!", AdminEmail: "admin@example.com",
		}))
		ok, err := svc.IsSystemInitialized(context.Background())
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("空邮箱保持 A1 放行口径（建号允许不填）", func(t *testing.T) {
		svc := newSetupService(t)
		require.NoError(t, svc.InitializeSystem(context.Background(), &InitializeSystemReq{
			AdminUsername: "admin1", AdminPassword: "secret123!", AdminEmail: "",
		}))
		ok, err := svc.IsSystemInitialized(context.Background())
		require.NoError(t, err)
		assert.True(t, ok)
	})
}
