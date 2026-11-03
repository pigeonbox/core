// chunk 通道「首片即拒」守卫（2026-11-03，与 presign PUT 预检同语义）。
//
// 背景：内容/魔数检查此前只在 Complete（合并后）做 ⇒ 伪装文件传完全程才被拒。
// 分片 0 即文件头部 ⇒ 首片预检可以在**第一个分片**就拒绝，不必等合并。
//
// 断言必须能变红（负对照）：
//   - 去掉 ChunkUpload 的首片预检 ⇒ …DisguisedExecutableRejectedAtFirstChunk 红；
//   - 清空 magicCheckSkipExts（旧实现）⇒ …AppImageFirstChunkAccepted 红（复现误杀）。
package chunk

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	chunkService "github.com/pigeonbox/core/app/chunk"
	"github.com/pigeonbox/core/repo/db/model"

	"github.com/pigeonbox/contracts/errcode"
)

// postChunk 以 multipart 表单投递一个分片，返回 HTTP 码与响应体。
func postChunk(t *testing.T, uploadID string, index int, filename string, data []byte) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition", `form-data; name="chunk"; filename="`+filename+`"`)
	hdr.Set("Content-Type", "application/octet-stream")
	part, err := w.CreatePart(hdr)
	require.NoError(t, err)
	_, err = part.Write(data)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.POST("/chunk/upload/chunk/:upload_id/:chunk_index", ChunkUpload)
	req := ut.PerformRequest(h.Engine, "POST",
		"/chunk/upload/chunk/"+uploadID+"/"+strconv.Itoa(index),
		&ut.Body{Body: strings.NewReader(buf.String()), Len: buf.Len()},
		ut.Header{Key: "Content-Type", Value: w.FormDataContentType()},
		ut.Header{Key: "X-Upload-Token", Value: chunkService.SessionToken(uploadID)})
	var out map[string]any
	_ = json.Unmarshal(req.Body.Bytes(), &out)
	return req.Code, out
}

// seedChunkSession 植入分片会话控制行（chunk_index = -1）、隔离工作目录，
// 返回测试 DB 供断言。
func seedChunkSession(t *testing.T, uploadID, fileName string) *gorm.DB {
	t.Helper()
	db := newIdemEnv(t)
	oldWd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(t.TempDir()))
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	require.NoError(t, db.Create(&model.UploadChunk{
		UploadID:    uploadID,
		ChunkIndex:  -1,
		FileName:    fileName,
		FileSize:    4096,
		TotalChunks: 2,
		ChunkSize:   2048,
		Status:      "uploading",
	}).Error)
	return db
}

func TestChunkUpload_DisguisedExecutableRejectedAtFirstChunk(t *testing.T) {
	const uploadID = "deadbeef01"
	db := seedChunkSession(t, uploadID, "cat.jpg")

	mz := []byte("MZ\x90\x00\x03fake-pe")
	code, body := postChunk(t, uploadID, 0, "cat.jpg", mz)
	assert.Equal(t, http.StatusUnsupportedMediaType, code,
		"首片即文件头部，伪装文件（cat.jpg 实为 PE）应在首片被拒（415），实际: %v", body)
	assert.Equal(t, float64(errcode.CodeFileTypeDenied), body["code"], "业务码应为 30011（不是 10008 内部错误）")

	// 被拒分片不得落存储记录
	var n int64
	require.NoError(t, db.Model(&model.UploadChunk{}).Where("upload_id = ? AND chunk_index = 0", uploadID).Count(&n).Error)
	assert.Equal(t, int64(0), n, "被拒分片不得记录")
}

func TestChunkUpload_AppImageFirstChunkAccepted(t *testing.T) {
	const uploadID = "deadbeef02"
	seedChunkSession(t, uploadID, "ZCode-3.15.3-linux-x64.AppImage")

	elf := []byte{0x7f, 'E', 'L', 'F', 0x02, 0x01, 0x01, 0x00}
	code, body := postChunk(t, uploadID, 0, "ZCode-3.15.3-linux-x64.AppImage", elf)
	assert.Equal(t, http.StatusOK, code,
		".AppImage 是诚实的 ELF 载体，首片应放行（旧实现在此误杀）: %v", body)
}

func TestChunkUpload_NonFirstChunkNotPreflighted(t *testing.T) {
	const uploadID = "deadbeef03"
	seedChunkSession(t, uploadID, "cat.jpg")

	// 非首片不做预检（分片 1 不是文件头部）——最终仍由 Complete 兜底复检拦住
	mz := []byte("MZ\x90\x00\x03fake-pe")
	code, body := postChunk(t, uploadID, 1, "cat.jpg", mz)
	assert.Equal(t, http.StatusOK, code, "预检只针对首片（分片 0）；后续分片照常收: %v", body)
}
