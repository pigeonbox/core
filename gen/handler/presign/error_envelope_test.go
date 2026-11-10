// core#114 A6b：gen/handler/presign 错误出口收编的 wire 判据。
// 参数错误 ⇒ 400 + 10001 + 全站信封（trace_id）。内部出口（生成预签名/中止上传）
// 已改 resp.NewInternalError（脱敏 + 进日志），行为由 pkg/resp 内部错误测试与
// AST 守卫（规则 B）共同兜底。
// 负对照（已实跑）：把 Abort 的 default 分支回退为 err.Error() 出网 ⇒ AST 守卫转红。
package presign

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pigeonbox/contracts/errcode"
)

func TestPresignErrorEnvelope_Param400(t *testing.T) {
	newHandlerEnv(t, &stubShareGateway{})
	code, body := postComplete(t, "not-json")
	assert.Equal(t, http.StatusBadRequest, code, "%v", body)
	assert.Equal(t, float64(errcode.CodeInvalidParam), body["code"], "%v", body)
	assert.NotEmpty(t, body["trace_id"], "错误响应必须带 trace_id：%v", body)
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t, []string{"code", "message", "trace_id"}, keys,
		"错误响应应是全站信封：%v", body)
}
