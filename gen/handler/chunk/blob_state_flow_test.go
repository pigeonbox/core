// 分片完成链的账本流转（core#172 第三片·拆分项 3）：
//
//	成功链：MergeUpload 后 committed 落行 → ShareFile/attachBlobRef 推进 active
//	失败链：魔数/扩展名复检失败、对象已删 ⇒ 账本 aborted
//
// 负对照（断言必须能变红）：
//   - 删掉完成链的 BlobEnsureCommitted 调用 ⇒ 成功用例拿不到行、
//     失功用例（Abort 是「只推进不建行」）也拿不到行 ⇒ 双红；
//   - 删掉 BlobAbort 调用 ⇒ 失功用例行停在 committed（对象已删的僵尸行）⇒ 红。
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
	"github.com/pigeonbox/core/storage"
)

// newBlobFlowEnv chunk + share 共库 + 真实 local 存储（合并要真落盘）。
func newBlobFlowEnv(t *testing.T) (*gorm.DB, *chunkService.Service) {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(
		&model.FileCode{}, &model.FileCodeFile{}, &model.PickupCharge{},
		&model.UploadChunk{}, &model.Blob{}, &model.FileReference{},
		&model.TransferLog{}, &model.SystemConfigRecord{},
	))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	t.Setenv("PB_PRESIGN_SIGNING_KEY", "test-presign-signing-key-0123456789")

	st, err := storage.NewStorageServiceE(&storage.StorageConfig{
		Type:     storage.StorageTypeLocal,
		DataPath: t.TempDir(),
	})
	require.NoError(t, err)

	cs := chunkService.NewService(g)
	cs.SetStorage(st)
	SetChunkService(cs)
	SetShareService(shareService.NewService(g, "http://test.local", nil))
	t.Cleanup(func() {
		SetChunkService(nil)
		SetShareService(nil)
	})
	return g, cs
}

// seedBlobFlowSession 控制行（chunk_index=-1，属主=直连对端）+ 已完成分片 + staging 数据。
func seedBlobFlowSession(t *testing.T, g *gorm.DB, cs *chunkService.Service, uploadID, fileName string, data []byte) {
	t.Helper()
	require.NoError(t, g.Create(&model.UploadChunk{
		UploadID:    uploadID,
		ChunkIndex:  -1,
		FileName:    fileName,
		FileSize:    int64(len(data)),
		TotalChunks: 1,
		ChunkSize:   len(data),
		OwnerIP:     "0.0.0.0",
		Status:      "uploading",
	}).Error)
	require.NoError(t, g.Create(&model.UploadChunk{
		UploadID:    uploadID,
		ChunkIndex:  0,
		FileName:    fileName,
		FileSize:    int64(len(data)),
		TotalChunks: 1,
		ChunkSize:   len(data),
		Completed:   true,
		Status:      "completed",
	}).Error)
	require.NoError(t, cs.SaveChunkData(t.Context(), uploadID, 0, data))
}

// postBlobComplete 走真实路由发 complete（与幂等用例同驱动形态）。
func postBlobComplete(t *testing.T, uploadID string) *ut.ResponseRecorder {
	t.Helper()
	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.POST("/chunk/upload/complete/:upload_id", ChunkUploadComplete)
	body := `{"expire_value":1,"expire_style":"count","require_auth":false}`
	return ut.PerformRequest(h.Engine, "POST", "/chunk/upload/complete/"+uploadID,
		&ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"},
		ut.Header{Key: "X-Upload-Token", Value: chunkService.SessionToken(uploadID)})
}

// TestChunkComplete_BlobLedgerActive 成功链：committed 落行 → 分享创建推进 active。
func TestChunkComplete_BlobLedgerActive(t *testing.T) {
	g, cs := newBlobFlowEnv(t)
	const uploadID = "BLOBFLOW-A1"
	data := []byte("hello pigeonbox blob ledger")
	seedBlobFlowSession(t, g, cs, uploadID, "hello.txt", data)

	w := postBlobComplete(t, uploadID)
	require.Equal(t, http.StatusOK, w.Code, "完成链应 200：%s", w.Body.String())
	var resp struct {
		Code int `json:"code"`
		Data struct {
			ShareCode string `json:"share_code"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Data.ShareCode, "应建成分享")

	// 账本：恰好一行，且走完 committed → active（EnsureBlobWithRef 沿链推进）。
	var blobs []model.Blob
	require.NoError(t, g.Find(&blobs).Error)
	require.Len(t, blobs, 1, "成功链应恰好一条 blob 行")
	assert.Equal(t, model.BlobActive, blobs[0].State,
		"committed 入口必须被分享创建推进到 active（卡在 committed = 推进断了）")
	assert.Equal(t, int64(len(data)), blobs[0].Size, "size 应是合并实测值")

	// 引用已挂（首片 attachBlobRef）。
	var refs int64
	require.NoError(t, g.Model(&model.FileReference{}).Count(&refs).Error)
	assert.Equal(t, int64(1), refs, "应挂一条引用")
}

// TestChunkComplete_BlobLedgerCommittedOnValidationFail 校验失败链（ensure 独占
// 可观测）：合并成功、账本已 committed，随后密码校验拒收（对象未删、分享未建）
// ⇒ 行必须停在 committed。此态只有完成链的落账能产生（分享创建还没发生，
// phase1 的 EnsureBlobWithRef 尚未介入）——抽掉 BlobEnsureCommitted 本用例必红。
func TestChunkComplete_BlobLedgerCommittedOnValidationFail(t *testing.T) {
	g, cs := newBlobFlowEnv(t)
	const uploadID = "BLOBFLOW-C1"
	data := []byte("hello committed state")
	seedBlobFlowSession(t, g, cs, uploadID, "hello.txt", data)

	h := server.New(server.WithHostPorts("127.0.0.1:0"))
	h.POST("/chunk/upload/complete/:upload_id", ChunkUploadComplete)
	body := `{"expire_value":1,"expire_style":"count","require_auth":true}` // 开密码但不给密码
	w := ut.PerformRequest(h.Engine, "POST", "/chunk/upload/complete/"+uploadID,
		&ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"},
		ut.Header{Key: "X-Upload-Token", Value: chunkService.SessionToken(uploadID)})
	require.NotEqual(t, http.StatusOK, w.Code, "缺密码应被拒：%s", w.Body.String())

	var blobs []model.Blob
	require.NoError(t, g.Find(&blobs).Error)
	require.Len(t, blobs, 1, "合并后校验失败必须留下账本行")
	assert.Equal(t, model.BlobCommitted, blobs[0].State,
		"对象未删、分享未建 ⇒ 应停在 committed（拿不到行 = ensure 没接）")

	var shares int64
	require.NoError(t, g.Model(&model.FileCode{}).Count(&shares).Error)
	assert.Equal(t, int64(0), shares, "缺密码不得建分享")
}

// TestChunkComplete_BlobLedgerAbortedOnReject 失败链（负对照）：扩展名黑名单
// 拒收、对象已删 ⇒ 账本必须是 aborted——它同时证明 committed 落行发生过
// （Abort 只推进不建行，没落行则本断言拿不到行）。
func TestChunkComplete_BlobLedgerAbortedOnReject(t *testing.T) {
	g, cs := newBlobFlowEnv(t)
	const uploadID = "BLOBFLOW-B1"
	data := []byte("MZ payload pretending to be an exe")
	// .exe 在默认黑名单（CheckUploadContent 第一道恒拒，不依赖 magic 开关）
	seedBlobFlowSession(t, g, cs, uploadID, "payload.exe", data)

	w := postBlobComplete(t, uploadID)
	require.NotEqual(t, http.StatusOK, w.Code, "黑名单扩展名应被拒：%s", w.Body.String())

	var blobs []model.Blob
	require.NoError(t, g.Find(&blobs).Error)
	require.Len(t, blobs, 1, "复检失败链应留下账本行（committed 落行发生过）")
	assert.Equal(t, model.BlobAborted, blobs[0].State,
		"对象已删 ⇒ 账本必须转 aborted（停在 committed = BlobAbort 没接）")

	// 分享绝不能建出。
	var shares int64
	require.NoError(t, g.Model(&model.FileCode{}).Count(&shares).Error)
	assert.Equal(t, int64(0), shares, "拒收链不得建分享")

	// 幂等负对照：拒收后重复 complete（会话已软删）仍不建分享、不新建行。
	w2 := postBlobComplete(t, uploadID)
	_ = w2
	var blobs2 int64
	require.NoError(t, g.Model(&model.Blob{}).Count(&blobs2).Error)
	assert.Equal(t, int64(1), blobs2, "重复 complete 不得新建账本行")
}
