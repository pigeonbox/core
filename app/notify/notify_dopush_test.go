// doPush 补偿壳回收的回归锁（core#85，2026-11-09）
//
// 上游 oekit/pkg httpjson#158 已修（v0.38.0 的 decodeOK 对 out==nil 跳过解码，
// 见 GOMODCACHE .../pkg@v0.38.0/httpjson/httpjson.go decodeOK）⇒ notify.doPush
// 的「RawMessage 中转 + SyntaxError 判成功」补偿壳已无存在必要，删除。
//
// 这两条都是回归锁：删除补偿壳前后行为**零变化**（2xx=送达 nil / 非 2xx=error），
// 任何一条红 = 补偿壳删错了东西。
package notify

import (
	"context"
	"errors"
	"testing"

	"github.com/oekit/pkg/httpjson"
	"github.com/stretchr/testify/require"
)

// TestDoPush_2xxEmptyBodyDelivered 2xx+空响应体 ⇒ nil（送达，响应体不关心）。
// 负对照：非 2xx 必须回 error（TestDoPush_Non2xxFails）。
func TestDoPush_2xxEmptyBodyDelivered(t *testing.T) {
	srv, _ := newRecordingServer(t, 200)
	req := httpjson.Request{Method: "POST", URL: srv.URL, Body: []byte(`{"event":"t"}`)}
	require.NoError(t, doPush(context.Background(), req))
}

// TestDoPush_Non2xxFails 非 2xx（500 且响应体为空）必须回 error——防止「一律
// 放行」式过度简化把 StatusError 语义也丢了。
func TestDoPush_Non2xxFails(t *testing.T) {
	srv, _ := newRecordingServer(t, 500)
	req := httpjson.Request{Method: "POST", URL: srv.URL, Body: []byte(`{"event":"t"}`)}
	err := doPush(context.Background(), req)
	require.Error(t, err)
	var se *httpjson.StatusError
	require.True(t, errors.As(err, &se), "错误链必须是 httpjson.StatusError，got %v", err)
}
