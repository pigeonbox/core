// core#110 wire 判据：/anonymous/generate 的 expire_value 非数字/≤0 不再被
// convert.Str2Int(.., 0) 静默当 0（day ⇒ 静默改义 1 天；count ⇒ 0 = 建完
// 即死——用户要 3 次拿到一个建完即死的分享，服务端还回 success），显式
// 400 + 10001 + 点名 expire_value。合法值行为不动（正判据）。
//
// 负对照（已实跑）：把 GenerateCode 的 ParseExpireParams 换回
// convert.Str2Int(req.GetExpireValue(), 0) ⇒ 「应拒绝」用例转红。
package share_anonymous

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	anonapp "github.com/pigeonbox/core/app/anonymous"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/repo/db/model"
)

// newGenerateEnv GenerateCode 最小环境：上传开放、匿名配额未启用、
// 匿名域内存 KV + sqlite。
func newGenerateEnv(t *testing.T) {
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
}

func performGenerate(t *testing.T, form string) (int, string) {
	t.Helper()
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.POST("/anonymous/generate", GenerateCode)
	w := ut.PerformRequest(h.Engine, http.MethodPost, "/anonymous/generate",
		&ut.Body{Body: strings.NewReader(form), Len: len(form)},
		ut.Header{Key: "Content-Type", Value: "application/x-www-form-urlencoded"},
		ut.Header{Key: "Host", Value: "test.local"})
	return w.Code, w.Body.String()
}

func TestGenerateCode_ExpireStrict(t *testing.T) {
	newGenerateEnv(t)

	t.Run("应拒绝：非数字/≤0", func(t *testing.T) {
		for _, form := range []string{
			"file_name=a.txt&file_size=10&expire_value=abc&expire_style=day",   // issue 点名：修前静默 1 天
			"file_name=a.txt&file_size=10&expire_value=abc&expire_style=count", // issue 点名：修前 0=建完即死
			"file_name=a.txt&file_size=10&expire_value=&expire_style=day",      // 显式空值 = 解析失败
			"file_name=a.txt&file_size=10&expire_value=0&expire_style=count",   // 建完即死，必须拒
			"file_name=a.txt&file_size=10&expire_value=-3&expire_style=day",
		} {
			code, body := performGenerate(t, form)
			assert.Equalf(t, http.StatusBadRequest, code, "%s 应 400: %s", form, body)
			assert.Containsf(t, body, "\"code\":10001", "%s 应参数错误业务码: %s", form, body)
			assert.Containsf(t, body, "expire_value", "%s 消息应点名参数: %s", form, body)
		}
	})

	t.Run("放行：合法值行为不动", func(t *testing.T) {
		code, body := performGenerate(t, "file_name=a.txt&file_size=10&expire_value=3&expire_style=day")
		require.Equalf(t, http.StatusOK, code, "合法值应成功: %s", body)
		var r struct {
			Code int `json:"code"`
			Data struct {
				ExpireSeconds int32 `json:"expire_seconds"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &r))
		assert.Equal(t, 0, r.Code, "业务码 0: %s", body)
		assert.InDelta(t, 3*24*3600, r.Data.ExpireSeconds, 60,
			"3 天过期语义不动（修前非数字才被静默改义）")

		// forever 免 ≤0 检查（strict 版语义）
		code, body = performGenerate(t, "file_name=b.txt&file_size=10&expire_value=0&expire_style=forever")
		assert.Equalf(t, http.StatusOK, code, "forever 应放行: %s", body)
	})
}
