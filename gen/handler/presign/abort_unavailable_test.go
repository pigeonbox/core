// abort_unavailable_test.go —— Abort 出口的「会话存储不可用」wire 判据。
//
// ## 为什么立这条
//
// 会话存储（Redis）故障时，presign 域上抛 typed error（errcode.CodeUnavailable
// 10006 ⇒ HTTP 503）。Init / Complete / UploadDirect 三个出口早已走
// resp.NewTypedError ⇒ 503；但 **Abort 出口的 default 分支写死了**
// resp.NewInternalError ⇒ 仍落 HTTP 500/10008 ⇒ 前端收到「中止上传失败」这种
// 笼统 500，无法分辨「服务暂时不可用（稍后重试）」与「参数非法」。
//
// 本用例把 Abort 的 default 分支钉成「typed error ⇒ 503」。
//
// 负对照（实跑）：把 Abort 的 default 分支改回单行
// resp.NewInternalError(c, errcode.CodeInternal, "中止上传", err) ⇒ 本用例转红
// （期望 503 实得 500、期望 10006 实得 10008）。
package presign

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"

	"github.com/pigeonbox/contracts/errcode"
	presignapp "github.com/pigeonbox/core/app/presign"
)

// TestAbort_RedisOutage_Returns503Unavailable 会话存储宕机 ⇒ Abort 出口
// 必须回 503 + 10006（服务不可用），而不是 500 + 10008（内部错误）。
//
// 注入的故障：redis 客户端指向一个**不监听**的端口（127.0.0.1:1）⇒ 任何命令
// 都立刻连接失败 ⇒ 等价「会话存储运行期宕机」。客户端参数关掉重试与退避
// （MaxRetries=-1 / DialerRetries=1），否则判据会退化成时序竞态。
func TestAbort_RedisOutage_Returns503Unavailable(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{
		Addr:          "127.0.0.1:1",
		MaxRetries:    -1,
		DialerRetries: 1,
		DialTimeout:   20 * time.Millisecond,
		ReadTimeout:   200 * time.Millisecond,
	})
	t.Cleanup(func() { _ = rdb.Close() })
	svc := presignapp.NewService(nil, rdb, "http://test.local", "test-signing-key-0123456789")
	SetService(svc)
	t.Cleanup(func() { SetService(nil) })

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.POST("/api/v1/presign/abort", Abort)
	body := `{"upload_id":"u-outage","token":"t-outage"}`
	w := ut.PerformRequest(h.Engine, "POST", "/api/v1/presign/abort",
		&ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code,
		"会话存储宕机时 Abort 必须回 503（服务暂时不可用），不该是笼统 500：%v", out)
	assert.Equal(t, float64(errcode.CodeUnavailable), out["code"],
		"业务码必须是 10006（CodeUnavailable）：%v", out)
}
