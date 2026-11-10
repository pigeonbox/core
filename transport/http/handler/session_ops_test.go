// /api/v1/user/refresh 的止损复核与 sid 继承（core#106）。
//
// 缺陷：refresh 此前只验签名/过期/黑名单，不走 identityFresh 的「纪元 +
// 会话存活」复核，且新令牌 sid 被洗空 ⇒ 改密 / 找回重置 / 退出所有设备 /
// 管理员重置口令（纪元 +1）的止损全部可被「旧令牌换新令牌」绕过，
// 被盗令牌无限续命。
//
// 负对照（已实跑）：
//   - 删掉 RefreshSession 里 auth.ParseToken + middleware.IdentityFresh 闸
//     ⇒「改密后 401」「会话已撤销 401」两条用例转红（拿到 200）；
//   - 把 pkg/auth.RefreshToken 的 GenerateTokenWithSession(..., claims.SessionID)
//     改回 GenerateToken ⇒「新令牌继承 sid」断言转红。
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/pkg/auth"
	"github.com/pigeonbox/core/pkg/middleware"
)

// TestRefreshSession_IdentityGate_And_SidInheritance 三段场景：
// ① 正常会话续签 200 且新令牌继承 sid；② 改密（纪元 +1）后旧令牌 401；
// ③ 会话被撤销（退出所有设备）后带 sid 令牌 401。
func TestRefreshSession_IdentityGate_And_SidInheritance(t *testing.T) {
	auth.SetJWTSecret("test-secret-refresh-gate-0123456789")
	t.Cleanup(func() { auth.SetJWTSecret("") })

	const uid uint = 9001
	epochNow := 0
	auth.SetEpochLoader(func(id uint) int {
		if id == uid {
			return epochNow
		}
		return 0
	})
	t.Cleanup(func() { auth.SetEpochLoader(nil) })

	rec := &middleware.IdentityRecord{
		Status:   "active",
		Role:     "user",
		Epoch:    0,
		Sessions: map[string]bool{"sid-alive": true},
	}
	middleware.SetIdentityLoader(func(ctx context.Context, id uint) (*middleware.IdentityRecord, error) {
		if id != uid {
			return nil, nil
		}
		return rec, nil
	})
	middleware.InvalidateIdentity(uid) // 清掉同包其它用例可能留下的 30s 缓存快照
	t.Cleanup(func() {
		middleware.InvalidateIdentity(uid)
		middleware.SetIdentityLoader(nil)
	})

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.POST("/api/v1/user/refresh", RefreshSession)

	do := func(tok string) (int, map[string]json.RawMessage) {
		t.Helper()
		w := ut.PerformRequest(h.Engine, http.MethodPost, "/api/v1/user/refresh", nil,
			ut.Header{Key: "Authorization", Value: "Bearer " + tok})
		var m map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &m), "响应必须是 JSON 对象: %s", w.Body.String())
		return w.Code, m
	}
	refreshedToken := func(t *testing.T, body map[string]json.RawMessage) string {
		t.Helper()
		var data map[string]string
		require.NoError(t, json.Unmarshal(body["data"], &data))
		require.NotEmpty(t, data["token"])
		return data["token"]
	}

	// ① 正常会话 ⇒ 200，新令牌继承 sid 且纪元与 DB 当前纪元一致
	tokOld, err := auth.GenerateTokenWithSession(uid, "alice", "user", "sid-alive")
	require.NoError(t, err)
	code, body := do(tokOld)
	require.Equal(t, http.StatusOK, code, "正常会话应可续签: %v", body)
	assert.Equal(t, "0", string(body["code"]))
	newClaims, err := auth.ParseToken(refreshedToken(t, body))
	require.NoError(t, err)
	assert.Equal(t, "sid-alive", newClaims.SessionID,
		"新令牌必须继承旧 sid（否则会话回查被洗掉、踢设备失效）")
	assert.Equal(t, 0, newClaims.Epoch)

	// ② 改密（纪元 +1）后，旧令牌续签必须 401（核心止损判据）
	tokBeforeBump, err := auth.GenerateTokenWithSession(uid, "alice", "user", "sid-alive")
	require.NoError(t, err)
	epochNow = 1 // ChangePassword/ResetPassword/RevokeAllSessions 都是纪元 +1
	rec.Epoch = 1
	middleware.InvalidateIdentity(uid)
	code, body = do(tokBeforeBump)
	assert.Equal(t, http.StatusUnauthorized, code,
		"改密后旧令牌不得续命（否则改密止损被 refresh 绕过）: %v", body)
	assert.Equal(t, "10002", string(body["code"]))

	// ③ 会话被撤销（退出所有设备语义：有效会话集合为空）⇒ 带 sid 令牌 401
	tokAfterRevoke, err := auth.GenerateTokenWithSession(uid, "alice", "user", "sid-alive")
	require.NoError(t, err)
	rec.Sessions = map[string]bool{}
	middleware.InvalidateIdentity(uid)
	code, body = do(tokAfterRevoke)
	assert.Equal(t, http.StatusUnauthorized, code,
		"会话已撤销的令牌不得续命: %v", body)
	assert.Equal(t, "10002", string(body["code"]))
}
