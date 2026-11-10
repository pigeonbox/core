// core#114 A6b：gen/handler/chunk 错误出口收编的 wire 判据。
//
// 判据：
//   - 内部（DB/存储）故障 ⇒ 500 + 10008 + 全站信封 + 内部细节不出网；
//   - 会话缺失等业务类保持 404 + 可操作文案 + 信封（trace_id）。
//
// 负对照（已实跑）：把 ChunkUploadInit 的「初始化上传失败」分支回退为
// legacy map + err.Error() ⇒ 内部脱敏判据转红（且 AST 守卫报规则 A/B）。
package chunk

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
	chunkService "github.com/pigeonbox/core/app/chunk"
	shareService "github.com/pigeonbox/core/app/share"
)

// newBrokenChunkEnv chunk/share 服务绑到**未建表**内存库 ⇒ InitiateUpload 等稳定失败。
func newBrokenChunkEnv(t *testing.T) {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	SetChunkService(chunkService.NewService(g))
	SetShareService(shareService.NewService(g, "http://test.local", nil))
	t.Cleanup(func() {
		SetChunkService(nil)
		SetShareService(nil)
	})
}

// postInitAs 以登录身份调 ChunkUploadInit（登录用户不过匿名闸门/配额，直达服务层）。
func postInitAs(t *testing.T, userID uint, body string) (int, map[string]any) {
	t.Helper()
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.POST("/chunk/upload/init/", func(ctx context.Context, c *app.RequestContext) {
		c.Set("user_id", userID)
		ChunkUploadInit(ctx, c)
	})
	w := ut.PerformRequest(h.Engine, "POST", "/chunk/upload/init/",
		&ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// postStatus 调 ChunkUploadStatus。
func postStatus(t *testing.T, uploadID string) (int, map[string]any) {
	t.Helper()
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.GET("/chunk/upload/status/:upload_id", ChunkUploadStatus)
	w := ut.PerformRequest(h.Engine, "GET", "/chunk/upload/status/"+uploadID, nil)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func assertChunkErrEnvelope(t *testing.T, body map[string]any, wantCode int, internalMarkers ...string) {
	t.Helper()
	assert.Equal(t, float64(wantCode), body["code"], "业务码：%v", body)
	assert.NotEmpty(t, body["trace_id"], "错误响应必须带 trace_id：%v", body)
	msg, _ := body["message"].(string)
	assert.NotEmpty(t, msg, "%v", body)
	for _, m := range internalMarkers {
		assert.NotContains(t, msg, m, "内部错误细节不得出网：%v", body)
	}
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t, []string{"code", "message", "trace_id"}, keys,
		"错误响应应是全站信封：%v", body)
}

func TestChunkErrorEnvelope_InternalSanitized(t *testing.T) {
	newBrokenChunkEnv(t)
	body := `{"file_name":"a.txt","file_hash":"deadbeef","file_size":10,"chunk_size":10,"total_chunks":1}`
	code, out := postInitAs(t, 1, body)
	assert.Equal(t, http.StatusInternalServerError, code,
		"坏库下 InitiateUpload 故障应 500：%v", out)
	assertChunkErrEnvelope(t, out, errcode.CodeInternal, "no such table", "SELECT", "sql:")
}

func TestChunkErrorEnvelope_MissingSession404(t *testing.T) {
	newBrokenChunkEnv(t)
	code, out := postStatus(t, "deadbeef01")
	assert.Equal(t, http.StatusNotFound, code,
		"会话缺失应 404（业务类保留状态与文案）：%v", out)
	assertChunkErrEnvelope(t, out, errcode.CodeNotFound)
}
