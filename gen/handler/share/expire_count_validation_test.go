// core#220：分享创建入口的 expire_value 下界校验（三态：0 / 负数 / 正常）。
//
// 修复前 handler 只校验 expire_style 白名单：count+0 落库 ExpiredCount=0
// （建完即死），count+负数 落库负值（IsCountExhausted 只判 ==0 ⇒ 变成无限次）。
// 本用例从 HTTP 入口钉住：0/负数必须 400 + 10001，正整数正常建分享。
package share

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/contracts/errcode"
	shareService "github.com/pigeonbox/core/app/share"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/repo/db/model"
)

func newExpireCountTestEnv(t *testing.T) {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}, &model.FileCodeFile{},
		&model.TransferLog{}, &model.PickupCharge{}))

	old := conf.GetGlobalConfig()
	conf.SetGlobalConfig(&conf.AppConfiguration{
		Upload:   conf.UploadConfig{OpenUpload: true},
		Transfer: conf.TransferConfig{MaxCount: 100, ExpireDefault: 7},
	})
	SetShareService(shareService.NewService(g, "http://test.local", nil))
	t.Cleanup(func() {
		SetShareService(nil)
		conf.SetGlobalConfig(old)
		_ = sqlDB.Close()
	})
}

func performShareText(t *testing.T, form string) (int, map[string]any) {
	t.Helper()
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.POST("/share/text/", func(ctx context.Context, c *app.RequestContext) {
		ShareText(ctx, c)
	})
	w := ut.PerformRequest(h.Engine, http.MethodPost, "/share/text/",
		&ut.Body{Body: strings.NewReader(form), Len: len(form)},
		ut.Header{Key: "Content-Type", Value: "application/x-www-form-urlencoded"})
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestShareText_CountExpireLowerBound(t *testing.T) {
	newExpireCountTestEnv(t)

	t.Run("count+0 ⇒ 400 拒绝（不得建完即死）", func(t *testing.T) {
		code, body := performShareText(t, "text=hello&expire_style=count&expire_value=0&require_auth=false")
		assert.Equal(t, http.StatusBadRequest, code, "%v", body)
		assert.Equal(t, float64(errcode.CodeInvalidParam), body["code"], "%v", body)
		msg, _ := body["message"].(string)
		assert.Contains(t, msg, "取件次数", "错误必须点名字段语义：%v", body)
	})

	t.Run("count+负数 ⇒ 400 拒绝（不得变成无限次）", func(t *testing.T) {
		for _, v := range []string{"-1", "-5"} {
			code, body := performShareText(t, "text=hello&expire_style=count&expire_value="+v+"&require_auth=false")
			assert.Equal(t, http.StatusBadRequest, code, "expire_value=%s: %v", v, body)
			assert.Equal(t, float64(errcode.CodeInvalidParam), body["code"], "%v", body)
		}
	})

	t.Run("count+正常值 ⇒ 200 且落库次数正确", func(t *testing.T) {
		code, body := performShareText(t, "text=hello&expire_style=count&expire_value=3&require_auth=false")
		require.Equal(t, http.StatusOK, code, "%v", body)
		assert.Equal(t, float64(0), body["code"], "%v", body)
	})
}
