// hub#364 强制测试①（L5 并发幂等）：100 个并发 Complete 打同一 upload_id
// ⇒ 最终只允许**一条分享记录 + 一份物理对象**。
//
// 与既有 2 并发回归（concurrent_complete_test.go，core#151）的区别：本用例把
// 并发量拉到 100（issue 正文的判据），并同刻断言分享行数与磁盘合并产物数。
// 环境复用 newConcurrentCompleteEnv（同包 helper）；#196 在途文件不含本文件。
//
// 正判据：100 方全部 200、拿到同一个 share_code、分享表该 upload_id 恰 1 行、
// uploads/ 下合并产物恰 1 份。
// 负对照：撤掉 handler 里的 uploadlock.Lock ⇒ 无锁窗口下多个并发方都过幂等
// 回读 ⇒ 分享行 ≥2 / 物理文件 ≥2 ⇒ 本用例转红（PR 里附实跑结果）。
package chunk

import (
	"bytes"
	"context"
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

// TestChunkUploadComplete_Concurrent100_OnlyOneShareOneObject 100 并发 Complete
// 同一 upload_id ⇒ 一条分享 + 一份物理对象 + 100 方同码。
func TestChunkUploadComplete_Concurrent100_OnlyOneShareOneObject(t *testing.T) {
	g, dataPath := newConcurrentCompleteEnv(t)
	ctx := context.Background()

	const uploadID = "CONC100-CHUNK-1"
	// 与 2 并发回归同量级：合并 + 整文件哈希需数毫秒，给无锁窗口留复现余量。
	content := bytes.Repeat([]byte("pigeonbox 100-way concurrent complete probe\n"), 80_000)
	fileSize := int64(len(content))

	require.NoError(t, g.Create(&model.UploadChunk{
		UploadID:    uploadID,
		ChunkIndex:  -1,
		FileName:    "race100.txt",
		FileSize:    fileSize,
		TotalChunks: 1,
		ChunkSize:   len(content),
		OwnerIP:     "0.0.0.0",
		Status:      "uploading",
	}).Error)
	require.NoError(t, g.Create(&model.UploadChunk{
		UploadID:   uploadID,
		ChunkIndex: 0,
		ChunkSize:  len(content),
		Completed:  true,
		OwnerIP:    "0.0.0.0",
		Status:     "completed",
	}).Error)
	require.NoError(t, getChunkService().SaveChunkData(ctx, uploadID, 0, content))

	// 单 engine 共享：hertz engine 本身按生产形态并发处理请求（ut 直连驱动、
	// 不起监听）；handler 走包内单例。
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.POST("/chunk/upload/complete/:upload_id", ChunkUploadComplete)

	body := `{"expire_value":1,"expire_style":"count","require_auth":false}`
	const n = 100
	start := make(chan struct{})
	var wg sync.WaitGroup
	statuses := make([]int, n)
	bodies := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Go(func() {
			<-start
			w := ut.PerformRequest(h.Engine, "POST", "/chunk/upload/complete/"+uploadID,
				&ut.Body{Body: strings.NewReader(body), Len: len(body)},
				ut.Header{Key: "Content-Type", Value: "application/json"},
				ut.Header{Key: "X-Upload-Token", Value: chunkService.SessionToken(uploadID)})
			statuses[i], bodies[i] = w.Code, w.Body.String()
		})
	}
	close(start)
	wg.Wait()

	for i := 0; i < n; i++ {
		require.Equal(t, 200, statuses[i], "第 %d 方 Complete 应成功：%s", i, bodies[i])
	}
	share0 := parseChunkShareCode(t, bodies[0])
	require.NotEmpty(t, share0)
	for i := 1; i < n; i++ {
		assert.Equal(t, share0, parseChunkShareCode(t, bodies[i]),
			"第 %d 方必须拿到同一个分享码（同属主幂等）", i)
	}

	var count int64
	require.NoError(t, g.Model(&model.FileCode{}).Where("upload_id = ?", uploadID).Count(&count).Error)
	assert.Equal(t, int64(1), count, "100 并发 Complete 只能建一条分享（否则取件额度翻 100 倍）")

	merged := countFilesUnder(t, dataPath+"/uploads")
	assert.Equal(t, 1, merged, "100 并发 Complete 只能合并出一份物理对象（无锁时各自生成 UUID 路径）")
}
