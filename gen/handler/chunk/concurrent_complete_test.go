// core#151 chunk 完成面并发竞态回归：同一 upload_id 并发 Complete 只能建
// **一条**分享、只合并出**一份**物理文件。
//
// 缺陷：handler 的「幂等回读 → 合并分片 → 建分享」是 read-then-create 且无锁
// ⇒ 并发双击/重放各自读到「未建过」⇒ 建 2 条分享（owner 的取件额度翻倍），
// MergeUpload 还各自生成新 UUID 路径 ⇒ 磁盘留两份。
//
// 正判据：两 goroutine 并发 POST /chunk/upload/complete/:upload_id ⇒
// 分享表该 upload_id 只有 1 行、两方拿到同一个 share_code、合并产物只有 1 份。
// 负对照：撤掉 handler 里的 uploadlock.Lock ⇒ 两方都过回读 ⇒ 行数=2 ⇒ 转红。
package chunk

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	chunkService "github.com/pigeonbox/core/app/chunk"
	shareService "github.com/pigeonbox/core/app/share"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/pigeonbox/core/storage"
)

// newConcurrentCompleteEnv chunk handler 并发完成的测试环境：
// 内存 sqlite + 注入 TempDir 存储（分片/合并产物不落仓内 ./data）+ 全局配置。
func newConcurrentCompleteEnv(t *testing.T) (*gorm.DB, string) {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}, &model.FileCodeFile{},
		&model.PickupCharge{}, &model.UploadChunk{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1) // :memory: 多连接会各见独立库
	t.Cleanup(func() { _ = sqlDB.Close() })

	// 会话令牌依赖 presign signing key（归属校验用）
	t.Setenv("PB_PRESIGN_SIGNING_KEY", "test-presign-signing-key-0123456789")

	dataPath := t.TempDir()
	st := storage.NewStorageService(&storage.StorageConfig{
		Type:     storage.StorageTypeLocal,
		DataPath: dataPath,
	})

	chunkSvc := chunkService.NewService(g)
	chunkSvc.SetStorage(st)
	SetChunkService(chunkSvc)
	SetShareService(shareService.NewService(g, "http://test.local", st))

	oldCfg := conf.GetGlobalConfig()
	conf.SetGlobalConfig(&conf.AppConfiguration{}) // 零值 = 配额/上限全不限
	t.Cleanup(func() {
		SetChunkService(nil)
		SetShareService(nil)
		conf.SetGlobalConfig(oldCfg)
	})
	return g, dataPath
}

// TestChunkUploadComplete_Concurrent_OnlyOneShare 并发 Complete 同一 upload_id：
// 只建一条分享、只合并一份物理文件、两方拿到同一个分享码。
func TestChunkUploadComplete_Concurrent_OnlyOneShare(t *testing.T) {
	g, dataPath := newConcurrentCompleteEnv(t)
	ctx := context.Background()

	const uploadID = "CONC-CHUNK-1"
	// 内容给并发窗口留余量：合并 + 整文件哈希需数毫秒，无锁时后到方必须在
	// 首个建分享之前完成回读才可能撞出第二条（体积越大窗口越稳）。
	content := bytes.Repeat([]byte("pigeonbox concurrent complete probe\n"), 80_000)
	fileSize := int64(len(content))

	// 上传会话：控制行（chunk_index=-1）+ 已完成分片行（completed=true
	// 才进 GetUploadedChunkIndexes）
	require.NoError(t, g.Create(&model.UploadChunk{
		UploadID:    uploadID,
		ChunkIndex:  -1,
		FileName:    "race.txt",
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
	// 分片数据落存储（与合并同一实例）
	require.NoError(t, getChunkService().SaveChunkData(ctx, uploadID, 0, content))

	body := `{"expire_value":1,"expire_style":"count","require_auth":false}`
	start := make(chan struct{})
	var wg sync.WaitGroup
	statuses := make([]int, 2)
	bodies := make([]string, 2)
	for i := range 2 {
		wg.Go(func() {
			<-start
			// 每方独立 engine（ut 直连驱动、不起监听）；handler 走包内单例
			h := server.New(server.WithHostPorts("127.0.0.1:0"))
			h.POST("/chunk/upload/complete/:upload_id", ChunkUploadComplete)
			w := ut.PerformRequest(h.Engine, "POST", "/chunk/upload/complete/"+uploadID,
				&ut.Body{Body: strings.NewReader(body), Len: len(body)},
				ut.Header{Key: "Content-Type", Value: "application/json"},
				ut.Header{Key: "X-Upload-Token", Value: chunkService.SessionToken(uploadID)})
			statuses[i], bodies[i] = w.Code, w.Body.String()
		})
	}
	close(start)
	wg.Wait()

	require.Equal(t, 200, statuses[0], "首个 Complete 应成功：%s", bodies[0])
	require.Equal(t, 200, statuses[1], "并发后到方应幂等成功，而不是报错：%s", bodies[1])
	shareA := parseChunkShareCode(t, bodies[0])
	shareB := parseChunkShareCode(t, bodies[1])
	require.NotEmpty(t, shareA)
	assert.Equal(t, shareA, shareB, "并发两方必须拿到同一个分享码（同属主幂等）")

	var count int64
	require.NoError(t, g.Model(&model.FileCode{}).Where("upload_id = ?", uploadID).Count(&count).Error)
	assert.Equal(t, int64(1), count, "并发 Complete 只能建一条分享（否则取件额度翻倍）")

	merged := countFilesUnder(t, filepath.Join(dataPath, "uploads"))
	assert.Equal(t, 1, merged, "合并产物只能有一份（无锁时 MergeUpload 各自生成新 UUID 路径 ⇒ 磁盘双份）")
}

// parseChunkShareCode 从 Complete 响应取 share_code。
func parseChunkShareCode(t *testing.T, body string) string {
	t.Helper()
	var resp struct {
		Code int `json:"code"`
		Data struct {
			ShareCode string `json:"share_code"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &resp))
	return resp.Data.ShareCode
}

// countFilesUnder 统计目录下文件数（不含目录本身）。
func countFilesUnder(t *testing.T, root string) int {
	t.Helper()
	n := 0
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	}))
	return n
}
