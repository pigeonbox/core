package chunk

// core#191 回归：chunk 完成面的**跨副本互斥**接线。
//
// ChunkUploadComplete 在进程内 uploadlock 之内再取 DB 级命名闸
// （app/chunk.Service.LockCompletion → repo/db.DLock）：多副本部署下同一
// upload_id 的并发 Complete 被串行化，正确性不再依赖进程锁。
//
// 本用例：N 个并发 Complete 同一 upload_id（命中幂等回读形态）——全链路经过
// 新的闸段代码路径；断言全部成功返回同一分享码、分享表仍只有一条。闸的
// 跨副本语义本身（MySQL GET_LOCK / PG advisory lock / sqlite 进程条纹）由
// repo/db/dblock_test.go 与 app/dbmigrate 真库验收覆盖。

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	chunkService "github.com/pigeonbox/core/app/chunk"
	"github.com/pigeonbox/core/repo/db/model"
)

// TestChunkUploadComplete_ConcurrentIdempotent 并发 Complete 同一 upload_id：
// 全部成功、同一分享码、分享表只有一条。
func TestChunkUploadComplete_ConcurrentIdempotent(t *testing.T) {
	g := newIdemEnv(t)

	const uploadID = "XREPLICA-CHUNK-1"
	// 已存在的分享 + 会话控制行（chunk_index = -1），与 complete_idempotent_test 同形态
	require.NoError(t, g.Create(&model.FileCode{
		Code:     "EXIST01",
		Text:     "big.zip",
		FilePath: "uploads",
		Size:     30,
		UploadID: uploadID,
		OwnerIP:  "0.0.0.0",
	}).Error)
	require.NoError(t, g.Create(&model.UploadChunk{
		UploadID:    uploadID,
		ChunkIndex:  -1,
		FileName:    "big.zip",
		FileSize:    30,
		TotalChunks: 1,
		ChunkSize:   30,
		Status:      "completed",
	}).Error)

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.POST("/chunk/upload/complete/:upload_id", ChunkUploadComplete)

	post := func() (int, int, string) {
		body := `{"expire_value":1,"expire_style":"count","require_auth":false}`
		w := ut.PerformRequest(h.Engine, "POST", "/chunk/upload/complete/"+uploadID,
			&ut.Body{Body: strings.NewReader(body), Len: len(body)},
			ut.Header{Key: "Content-Type", Value: "application/json"},
			ut.Header{Key: "X-Upload-Token", Value: chunkService.SessionToken(uploadID)})
		var resp struct {
			Code int `json:"code"`
			Data struct {
				ShareCode string `json:"share_code"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), "body: %s", w.Body.String())
		return w.Code, resp.Code, resp.Data.ShareCode
	}

	var (
		wg    sync.WaitGroup
		codes = make([]string, 6)
		biz   = make([]int, 6)
		https = make([]int, 6)
	)
	for i := range 6 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			https[i], biz[i], codes[i] = post()
		}(i)
	}
	wg.Wait()

	for i := range 6 {
		require.Equal(t, http.StatusOK, https[i], "并发方 %d HTTP 状态应 200", i)
		require.Equal(t, 0, biz[i], "并发方 %d 业务码应 0（成功幂等）", i)
		assert.Equal(t, "EXIST01", codes[i], "并发方 %d 必须复用同一分享码", i)
	}

	var count int64
	require.NoError(t, g.Table("file_codes").Where("upload_id = ?", uploadID).Count(&count).Error)
	assert.EqualValues(t, 1, count, "并发 Complete 不得新建分享")
}
