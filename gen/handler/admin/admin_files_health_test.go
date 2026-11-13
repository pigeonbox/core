// GET /admin/files 的 health 参数**严格校验**守卫（core#147）。
//
// 缺陷形态：health 直接进 dao 的 applyHealthFilter switch，default 分支**静默丢条件**
// ⇒ 未知取值（仪表盘直达链接 ?health=expiringSoon 这类拼写/大小写错）
// ⇒ 返回**全量**分享，而调用方以为看的是「24h 内到期」。
//
// 判据：未知 health ⇒ 400 + 10001 + 点名参数；合法 health ⇒ 校验放行、照旧进 service。
// 负对照：把 ValidateHealthFilter 的调用删掉 ⇒ 「应拒绝」用例转红（坏库下会拿到 500）。
package admin

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/contracts/errcode"
)

// doAdminFilesReq 打一次 GET /admin/files（本组只测参数校验，鉴权在路由层）。
func doAdminFilesReq(t *testing.T, query string) (int, map[string]any) {
	t.Helper()
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.GET("/admin/files", AdminListFiles)

	path := "/admin/files"
	if query != "" {
		path += "?" + query
	}
	w := ut.PerformRequest(h.Engine, "GET", path, nil)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestAdminListFiles_UnknownHealth_400(t *testing.T) {
	newBrokenAdminEnv(t)

	// page 是契约必填（BindAndValidate 先于 health 校验），必须带上才测得到目标分支。
	for _, bad := range []string{"page=1&page_size=20&health=expiringSoon", "page=1&page_size=20&health=Expiredd", "page=1&page_size=20&health=soon"} {
		code, body := doAdminFilesReq(t, bad)
		assert.Equal(t, http.StatusBadRequest, code,
			"未知 health=%s 应 400（不得静默丢条件返回全量）：%v", bad, body)
		assert.Equal(t, float64(errcode.CodeInvalidParam), body["code"], "%v", body)
		msg, _ := body["message"].(string)
		assert.Contains(t, msg, "health", "错误消息必须点名参数：%v", body)
	}
}

func TestAdminListFiles_ValidHealth_PassesValidation(t *testing.T) {
	newBrokenAdminEnv(t)

	// 合法 health：校验必须放行 ⇒ 走到 service（坏库 ⇒ 500）。
	// 若校验过严把合法值也挡了，这里会变成 400。
	for _, good := range []string{"page=1&page_size=20&health=active", "page=1&page_size=20&health=expiring_soon", "page=1&page_size=20&health=forever", "page=1&page_size=20"} {
		code, body := doAdminFilesReq(t, good)
		require.NotEqual(t, http.StatusBadRequest, code,
			"合法 health=%q 不得被参数校验拦下：%v", good, body)
	}
}
