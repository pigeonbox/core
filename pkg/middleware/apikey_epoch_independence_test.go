package middleware

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/stretchr/testify/assert"
)

// 守卫（fnos#182 验收项之一：「旧 API Key 仍能 200」）：
//
// API Key 是**独立凭证**——鉴权链路（validateAPIKey）不参与会话纪元/账号状态
// 回查（identity 回查只作用于 JWT 路径）。
//
// 为什么必须锁死：管理员原地改口令会让会话纪元 +1（旧 JWT 即时失效），
// 若 Key 路径也要过纪元复核 ⇒ 管理员改一次口令，
// 所有已签发 Key（集成方、MCP、Beam 出厂预接线那把）**全部失效**。
//
// 判据（能判红）：注入一个**必然失败**的 identity loader ——
// 若 Key 路径改为要过 identity 回查，这里就会失败（loader 被调用 ⇒ 请求被拒）。
func TestAPIKeyAuth_DoesNotConsultIdentityGuard(t *testing.T) {
	g := newAPIKeyTestEnv(t)
	u := newFixtureUser(t, g, "active")
	plain, _ := newFixtureKey(t, g, u.ID, nil)

	useFreshIdentityCache(t, nil)
	SetIdentityLoader(func(ctx context.Context, userID uint) (*IdentityRecord, error) {
		return nil, errors.New("api key path must not consult the identity guard")
	})
	t.Cleanup(func() { SetIdentityLoader(nil) })

	// 生产同款链路：可选 JWT（此处无 token ⇒ 匿名放行）+ API Key 认证
	chain := append([]app.HandlerFunc{}, OptionalIdentity()...)
	code, body, identity, _ := performProbe(t, chain, "/probe",
		ut.Header{Key: "X-API-Key", Value: plain})

	assert.Equal(t, http.StatusOK, code,
		"API Key 鉴权不得依赖会话纪元/账号状态回查（改密后旧 Key 仍须可用）: %s", body)
	assert.Equal(t, "api_key", identity["auth_type"], "应当以 API Key 身份通过")
	assert.Equal(t, u.ID, identity["user_id"], "身份应当是 Key 属主")
}
