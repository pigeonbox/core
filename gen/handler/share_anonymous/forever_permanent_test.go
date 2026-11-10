// core#137 wire 判据：/anonymous/generate 的 expire_style=forever 建出的分享
// 必须**真不过期**（file_codes.expired_at = NULL = 永久——DB 真相源：IsExpired()
// 恒 false、janitor 按 expired_at < now 清理天然跳过、管理台列表显示「永久」）。
//
// 修复前行为：forever 时 utils.CalculateExpireTime 返回 nil（=显式永不过期）被
// 丢弃、expireAt 保持默认 24h ⇒ expired_at = now+24h——用户选「永久」拿到的是
// 24 小时过期的分享（静默改义）。
//
// 负对照（已实跑）：把 GenerateCode 的 forever 分支改回
// `if t := utils.CalculateExpireTime(...); t != nil { expireAt = *t }`（nil 丢弃）
// ⇒ 两个「forever 必须落 NULL」判据转红；还原后全绿。
package share_anonymous

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	anonapp "github.com/pigeonbox/core/app/anonymous"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/repo/db/model"
)

// newForeverEnv 与 expire_strict_test.go 的 newGenerateEnv 同构，但把 *gorm.DB
// 交回测试以便断言落库行的 expired_at（判据：永久 = NULL）。
func newForeverEnv(t *testing.T) *gorm.DB {
	t.Helper()
	old := conf.GetGlobalConfig()
	conf.SetGlobalConfig(&conf.AppConfiguration{Upload: conf.UploadConfig{OpenUpload: true}})
	t.Cleanup(func() { conf.SetGlobalConfig(old) })

	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}, &model.FileCodeFile{},
		&model.PickupCharge{}, &model.TransferLog{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1) // :memory: 每连接独立库，钉单连接防 flake
	SetService(anonapp.NewService(nil, g))
	return g
}

// rowByFileName 按落库行的展示名（SanitizeFileName 后的原始名）取唯一行。
func rowByFileName(t *testing.T, g *gorm.DB, name string) *model.FileCode {
	t.Helper()
	var fc model.FileCode
	require.NoErrorf(t, g.Where("uuid_file_name = ?", name).First(&fc).Error, "应有 %s 的落库行", name)
	return &fc
}

type foreverGenResp struct {
	Code int `json:"code"`
	Data struct {
		Code          string `json:"code"`
		ExpireSeconds int32  `json:"expire_seconds"`
	} `json:"data"`
}

func TestGenerateCode_ForeverIsPermanent(t *testing.T) {
	g := newForeverEnv(t)

	t.Run("forever+0 ⇒ 永久（DB NULL）+ expire_seconds=-1", func(t *testing.T) {
		code, body := performGenerate(t, "file_name=perm-a.txt&file_size=10&expire_value=0&expire_style=forever")
		require.Equalf(t, http.StatusOK, code, "forever 应成功: %s", body)
		var r foreverGenResp
		require.NoError(t, json.Unmarshal([]byte(body), &r))
		assert.Equal(t, 0, r.Code, "业务码 0: %s", body)
		assert.Equal(t, int32(-1), r.Data.ExpireSeconds, "永久的剩余秒数 = -1（无限，与 max_pickup_count=-1 同口径）")

		fc := rowByFileName(t, g, "perm-a.txt")
		assert.Nil(t, fc.ExpiredAt, "forever 必须落 expired_at NULL（永久）——修前落的是 now+24h")
		assert.False(t, fc.IsExpired())
	})

	t.Run("只传 style=forever（前端「永久」形态）⇒ 同样永久", func(t *testing.T) {
		code, body := performGenerate(t, "file_name=perm-b.txt&file_size=10&expire_style=forever")
		require.Equalf(t, http.StatusOK, code, "style-only forever 应成功: %s", body)
		var r foreverGenResp
		require.NoError(t, json.Unmarshal([]byte(body), &r))
		assert.Equal(t, int32(-1), r.Data.ExpireSeconds)

		fc := rowByFileName(t, g, "perm-b.txt")
		assert.Nil(t, fc.ExpiredAt, "style-only forever 同样必须落 NULL")
		assert.False(t, fc.IsExpired())
	})

	t.Run("历史口径不动：未指定 ⇒ 默认 24h；day/3 ⇒ 3 天", func(t *testing.T) {
		code, body := performGenerate(t, "file_name=def-c.txt&file_size=10")
		require.Equalf(t, http.StatusOK, code, "%s", body)
		fc := rowByFileName(t, g, "def-c.txt")
		require.NotNil(t, fc.ExpiredAt, "完全未指定过期仍默认 24h（历史口径不动）")
		assert.InDelta(t, 24*3600, time.Until(*fc.ExpiredAt).Seconds(), 60)

		code, body = performGenerate(t, "file_name=day-d.txt&file_size=10&expire_value=3&expire_style=day")
		require.Equalf(t, http.StatusOK, code, "%s", body)
		fc = rowByFileName(t, g, "day-d.txt")
		require.NotNil(t, fc.ExpiredAt, "day/3 仍 3 天（A4 core#110 口径不动）")
		assert.InDelta(t, 72*3600, time.Until(*fc.ExpiredAt).Seconds(), 60)
	})
}
