// chunk 完成面的**幂等**回归（2026-10-24 审计，core v0.32.6）
//
// 缺陷：ChunkUploadComplete **不检查**是否已建过分享 ⇒ 同一 upload_id 二次
// complete（双击 / 客户端重试 / 网络重试）会**再建一条分享**：
//   - 两个分享各有**独立的取件次数额度**与取件码 ⇒ owner 设的「最多取 N 次」
//     实际可取 **2N** 次（直接击穿取件计次承诺）；
//   - 用户存储统计被双计；
//   - 每次合并生成新的 UUID 物理路径 ⇒ 磁盘上留下第二份文件。
//
// 真机实测：13 条带 upload_id 的分享里已有 **1 组重复** ⇒ 已在生产数据发生。
//
// 修法：以 upload_id 为**幂等键** —— 合并之前先回读已建分享，命中即复用
// （同一个分享码），不新建行、不重复扣次、不重复计配额、不再合并。
//
// 断言必须能变红（负对照）：删掉 handler 里的幂等回读 ⇒ 本用例变红。
package chunk

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	chunkService "github.com/pigeonbox/core/app/chunk"
	shareService "github.com/pigeonbox/core/app/share"
	"github.com/pigeonbox/core/repo/db/model"
)

// newIdemEnv 构造 chunk handler 测试环境（chunk + share 共用一个 sqlite）。
func newIdemEnv(t *testing.T) *gorm.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}, &model.FileCodeFile{},
		&model.PickupCharge{}, &model.UploadChunk{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	// 会话令牌依赖 presign signing key（归属校验用）
	t.Setenv("PB_PRESIGN_SIGNING_KEY", "test-presign-signing-key-0123456789")

	SetChunkService(chunkService.NewService(g))
	SetShareService(shareService.NewService(g, "http://test.local", nil))
	t.Cleanup(func() {
		SetChunkService(nil)
		SetShareService(nil)
	})
	return g
}

// TestChunkUploadComplete_Idempotent 同一 upload_id 已建过分享 ⇒ 幂等返回
// 同一个分享码，**不新建**第二条分享。
func TestChunkUploadComplete_Idempotent(t *testing.T) {
	g := newIdemEnv(t)

	const uploadID = "IDEM-CHUNK-1"
	// 已存在的分享（同一 upload_id）—— 模拟「第一次 complete 已建成」
	require.NoError(t, g.Create(&model.FileCode{
		Code:         "EXIST01",
		Text:         "big.zip",
		FilePath:     "uploads",
		UUIDFileName: "big.zip",
		Size:         30,
		UploadID:     uploadID,
	}).Error)
	// 上传会话控制记录（chunk_index = -1 才是控制行，见 dao.GetByUploadID）
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
	body := `{"expire_value":1,"expire_style":"count","require_auth":false}`
	w := ut.PerformRequest(h.Engine, "POST", "/chunk/upload/complete/"+uploadID,
		&ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"},
		ut.Header{Key: "X-Upload-Token", Value: chunkService.SessionToken(uploadID)})

	require.Equal(t, http.StatusOK, w.Code, "幂等路径应返回 200：%s", w.Body.String())
	var resp struct {
		Code int `json:"code"`
		Data struct {
			ShareCode string `json:"share_code"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "EXIST01", resp.Data.ShareCode, "必须复用已有分享码")

	// 关键：分享表里**仍只有一条**（不得新建）
	var count int64
	require.NoError(t, g.Model(&model.FileCode{}).Where("upload_id = ?", uploadID).Count(&count).Error)
	assert.Equal(t, int64(1), count, "二次 complete 不得再建一条分享（否则取件额度翻倍）")
}
