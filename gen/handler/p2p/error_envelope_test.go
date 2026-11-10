// core#114 A6b：gen/handler/p2p 错误出口收编的 wire 判据。
// 参数错误 ⇒ 400 + 10001 + 全站信封（trace_id）；成功面 wire 形状不动。
// 负对照（已实跑）：把 PostP2PInvite 的 bind 错误回退为契约模型 legacy 形态 ⇒ 转红。
package p2p

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/stretchr/testify/assert"

	"github.com/pigeonbox/contracts/errcode"
)

func TestP2PInviteErrorEnvelope_Param400(t *testing.T) {
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.POST("/api/v1/p2p/invite", PostP2PInvite)

	w := ut.PerformRequest(h.Engine, "POST", "/api/v1/p2p/invite",
		&ut.Body{Body: strings.NewReader("not-json"), Len: len("not-json")},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)

	assert.Equal(t, http.StatusBadRequest, w.Code, "%v", body)
	assert.Equal(t, float64(errcode.CodeInvalidParam), body["code"], "%v", body)
	assert.NotEmpty(t, body["trace_id"], "错误响应必须带 trace_id：%v", body)
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t, []string{"code", "message", "trace_id"}, keys,
		"错误响应应是全站信封（不再用契约模型 legacy 形态）：%v", body)
}
