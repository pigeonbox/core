// 上传会话账本的生产流转回归（core#172 第三片·拆分项 3）：
// Pending→Uploading→Committed→Active 全链、Complete 幂等负对照、
// s3 方案多跳推进、失败链（过期/中止）转 aborted。
//
// 断言必须能变红（负对照）：
//
//	① 删掉 Init 的 pending 落行 ⇒ TestUploadState_SelfScheme_FullChain 变红；
//	② 删掉 UploadDirect 的两跳推进 ⇒ 同上（状态停 pending）；
//	③ 删掉 Complete 的幂等回读 ⇒ TestUploadState_CompleteIdempotentLedger
//	  的「行数/引用数不涨」变红；
//	④ 把状态机改成允许回退 ⇒ 各 rejected 断言变红。
package presign

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/pigeonbox/core/pkg/metrics"
	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// memStorage 内存对象存储：StorageWriter（SaveBytes/DeleteFile）+ headReader
// （Complete 3.6 存在性核实 / 3.8 魔数复检都要读头）。
type memStorage struct {
	mu    sync.Mutex
	files map[string][]byte
}

func newMemStorage() *memStorage { return &memStorage{files: map[string][]byte{}} }

func (m *memStorage) SaveBytes(_ context.Context, path string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	m.files[path] = cp
	return nil
}

func (m *memStorage) DeleteFile(_ context.Context, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.files, path)
	return nil
}

func (m *memStorage) GetFileReader(_ context.Context, path string) (io.ReadCloser, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.files[path]
	if !ok {
		return nil, 0, fmt.Errorf("object not found: %s", path)
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

// attachingShare 模拟 share 域的 attachBlobRef（首片 CreateShare 双写）：
// 建分享行 + EnsureBlobWithRef 挂引用，回读按「建过即返回」。
type attachingShare struct {
	repo *dao.BlobRepository
	g    *gorm.DB

	mu       sync.Mutex
	codes    []string
	lastCode string
}

func (a *attachingShare) CreateShare(ctx context.Context, req *ShareCreateReq) (*ShareCreateResp, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	fc := &model.FileCode{Code: fmt.Sprintf("P3S%05d", len(a.codes)+1), Text: req.Text}
	if err := a.g.Create(fc).Error; err != nil {
		return nil, err
	}
	// 与首片 attachBlobRef 同语义（blobRepo.EnsureBlobWithRef）
	if _, err := a.repo.EnsureBlobWithRef(ctx, req.FilePath, req.Size, req.FileHash, fc.ID); err != nil {
		return nil, err
	}
	a.codes = append(a.codes, fc.Code)
	a.lastCode = fc.Code
	return &ShareCreateResp{
		Code:         fc.Code,
		ShareURL:     "/share/" + fc.Code,
		FullShareURL: "http://test.local/share/" + fc.Code,
	}, nil
}

func (a *attachingShare) FindShareByUploadIDOwnedBy(_ context.Context, _ string, _ *uint, _ string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastCode, nil // 真实 share 域按 upload_id 命中即复用
}

// newUploadStateService 带 blobs/file_references 迁移的测试装配
// （newTestService 的库不迁账本表——本文件要对账本行断言，单独装配）。
func newUploadStateService(t *testing.T) (*Service, *gorm.DB) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	g := newPresignTestDB(t)
	require.NoError(t, g.AutoMigrate(&model.Blob{}, &model.FileReference{}, &model.FileCode{}))
	return NewService(g, rdb, "http://test.local", "test-signing-key"), g
}

// blobRowOf 按 object_key 读账本行。
func blobRowOf(t *testing.T, g *gorm.DB, key string) model.Blob {
	t.Helper()
	var b model.Blob
	require.NoError(t, g.First(&b, "object_key = ?", key).Error)
	return b
}

func countRows(t *testing.T, g *gorm.DB, m any, key string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, g.Model(m).Where("object_key = ?", key).Count(&n).Error)
	return n
}

// 全链（self 方案）：Init 落 pending → PUT 两跳到 committed → Complete 挂引用到 active。
func TestUploadState_SelfScheme_FullChain(t *testing.T) {
	svc, g := newUploadStateService(t)
	st := newMemStorage()
	svc.SetStorage(st)
	share := &attachingShare{repo: dao.NewBlobRepository(g), g: g}
	svc.SetShareService(share)
	ctx := context.Background()

	mCreated0 := metrics.BlobUploadTransitionsValue("none", model.BlobPending, dao.UploadTxCreated)
	mPU0 := metrics.BlobUploadTransitionsValue(model.BlobPending, model.BlobUploading, dao.UploadTxAdvanced)
	mUC0 := metrics.BlobUploadTransitionsValue(model.BlobUploading, model.BlobCommitted, dao.UploadTxAdvanced)

	initRes, err := svc.Init(ctx, InitMeta{FileName: "chain.txt", FileSize: 8})
	require.NoError(t, err)
	key := initRes.ObjectKey

	// Init ⇒ pending（对象已定位、写未开始）
	require.Equal(t, model.BlobPending, blobRowOf(t, g, key).State, "Init 必须落 pending 行")
	require.Equal(t, int64(8), blobRowOf(t, g, key).Size, "申报大小应落账")

	// PUT ⇒ uploading → committed
	content := []byte("hello-p3")
	require.NoError(t, svc.UploadDirect(ctx, initRes.UploadID, initRes.Token, bytes.NewReader(content), 8))
	require.Equal(t, model.BlobCommitted, blobRowOf(t, g, key).State,
		"UploadDirect 成功后必须到 committed")

	// Complete ⇒ 分享挂引用，committed → active（首片 EnsureBlobWithRef）
	res, err := svc.Complete(ctx, initRes.UploadID, initRes.Token, "1.2.3.4")
	require.NoError(t, err)
	require.NotEmpty(t, res.ShareCode)
	require.Equal(t, model.BlobActive, blobRowOf(t, g, key).State, "完整链终点必须 active")

	var refs int64
	require.NoError(t, g.Model(&model.FileReference{}).Count(&refs).Error)
	require.Equal(t, int64(1), refs, "一条分享一条引用")

	// 指标增量：created×1、pending→uploading×1、uploading→committed×1
	assert.Equal(t, mCreated0+1, metrics.BlobUploadTransitionsValue("none", model.BlobPending, dao.UploadTxCreated))
	assert.Equal(t, mPU0+1, metrics.BlobUploadTransitionsValue(model.BlobPending, model.BlobUploading, dao.UploadTxAdvanced))
	assert.Equal(t, mUC0+1, metrics.BlobUploadTransitionsValue(model.BlobUploading, model.BlobCommitted, dao.UploadTxAdvanced))
}

// Complete 幂等负对照：二次 Complete 复用同一分享，账本行/引用不涨；
// 且发布后（active）任何回退推进被状态机拒绝。
func TestUploadState_CompleteIdempotentLedger(t *testing.T) {
	svc, g := newUploadStateService(t)
	svc.SetStorage(newMemStorage())
	share := &attachingShare{repo: dao.NewBlobRepository(g), g: g}
	svc.SetShareService(share)
	ctx := context.Background()

	initRes, err := svc.Init(ctx, InitMeta{FileName: "idem.txt", FileSize: 8})
	require.NoError(t, err)
	key := initRes.ObjectKey
	require.NoError(t, svc.UploadDirect(ctx, initRes.UploadID, initRes.Token, bytes.NewReader([]byte("hello-p3")), 8))

	first, err := svc.Complete(ctx, initRes.UploadID, initRes.Token, "1.2.3.4")
	require.NoError(t, err)
	require.Equal(t, model.BlobActive, blobRowOf(t, g, key).State)

	// 二次 Complete：同码、不再建分享、账本零增长
	second, err := svc.Complete(ctx, initRes.UploadID, initRes.Token, "1.2.3.4")
	require.NoError(t, err, "二次 Complete 应幂等")
	assert.Equal(t, first.ShareCode, second.ShareCode, "二次必须返回同一分享码")
	assert.Equal(t, 1, len(share.codes), "二次 Complete 不得再建分享")
	assert.Equal(t, int64(1), countRows(t, g, &model.Blob{}, key), "blob 行不得翻倍")

	var refCount int64
	require.NoError(t, g.Model(&model.FileReference{}).Count(&refCount).Error)
	assert.Equal(t, int64(1), refCount, "引用行不得翻倍")

	// 负对照：发布后回退（active → uploading/committed）必须被拒
	repo := dao.NewBlobRepository(g)
	for _, target := range []string{model.BlobUploading, model.BlobCommitted} {
		_, outcome, advErr := repo.AdvanceUploadBlob(ctx, key, target)
		require.ErrorIs(t, advErr, dao.ErrBlobNotReusable, "active → %s 必须拒绝", target)
		assert.Equal(t, dao.UploadTxRejected, outcome)
	}
	assert.Equal(t, model.BlobActive, blobRowOf(t, g, key).State, "拒绝后状态不得被改写")
}

// s3 方案：服务端不见流量，Complete 以 HeadObject 为据沿
// pending→uploading→committed 逐跳推进（单次调用多跳走完）。
func TestUploadState_S3Scheme_WalksToCommitted(t *testing.T) {
	svc, g := newUploadStateService(t)
	obj := &fakeObjectStore{putURL: "https://s3.test.local/put/", headObj: map[string]int64{}}
	svc.SetObjectStore(obj)
	share := &attachingShare{repo: dao.NewBlobRepository(g), g: g}
	svc.SetShareService(share)
	ctx := context.Background()

	mWalk0 := metrics.BlobUploadTransitionsValue(model.BlobPending, model.BlobCommitted, dao.UploadTxAdvanced)

	initRes, err := svc.Init(ctx, InitMeta{FileName: "s3.txt", FileSize: 2048})
	require.NoError(t, err)
	require.Equal(t, SchemeS3, initRes.Scheme, "桩必须让 Init 走 s3 方案")
	key := initRes.ObjectKey
	require.Equal(t, model.BlobPending, blobRowOf(t, g, key).State)
	// 对象已在对象存储（流量不经本进程），Complete 3.5 HeadObject 以它为据
	obj.headObj[key] = 2048

	// 无 UploadDirect（流量不过本进程），直接 Complete
	res, err := svc.Complete(ctx, initRes.UploadID, initRes.Token, "1.2.3.4")
	require.NoError(t, err)
	require.NotEmpty(t, res.ShareCode)
	require.Equal(t, model.BlobActive, blobRowOf(t, g, key).State,
		"s3 方案必须经 committed 推进后挂引用到 active")
	assert.Equal(t, mWalk0+1, metrics.BlobUploadTransitionsValue(model.BlobPending, model.BlobCommitted, dao.UploadTxAdvanced),
		"多跳推进应记一笔 pending→committed advanced")

	var refs int64
	require.NoError(t, g.Model(&model.FileReference{}).Count(&refs).Error)
	assert.Equal(t, int64(1), refs)
}

// 失败链：过期拒收 ⇒ aborted；显式 Abort ⇒ aborted（行存在时）。
func TestUploadState_FailurePaths_Aborted(t *testing.T) {
	t.Run("过期拒收", func(t *testing.T) {
		svc, g := newUploadStateService(t)
		svc.SetStorage(newMemStorage())
		svc.SetShareService(&mockShareService{returnCode: "EXP0001"})
		ctx := context.Background()

		initRes, err := svc.Init(ctx, InitMeta{FileName: "exp.txt", FileSize: 8})
		require.NoError(t, err)
		key := initRes.ObjectKey
		require.Equal(t, model.BlobPending, blobRowOf(t, g, key).State)

		// 推进时钟越过 ExpireAt（数据面未传完 ⇒ 拒收）
		svc.nowFn = func() time.Time { return time.Now().Add(2 * time.Hour) }
		_, err = svc.Complete(ctx, initRes.UploadID, initRes.Token, "1.2.3.4")
		require.ErrorIs(t, err, ErrUploadExpired)
		require.Equal(t, model.BlobAborted, blobRowOf(t, g, key).State, "过期拒收必须转 aborted")

		// 负对照：aborted 不复活
		_, outcome, advErr := dao.NewBlobRepository(g).AdvanceUploadBlob(ctx, key, model.BlobUploading)
		require.ErrorIs(t, advErr, dao.ErrBlobNotReusable)
		assert.Equal(t, dao.UploadTxRejected, outcome)
	})

	t.Run("显式中止", func(t *testing.T) {
		svc, g := newUploadStateService(t)
		svc.SetStorage(newMemStorage())
		ctx := context.Background()

		initRes, err := svc.Init(ctx, InitMeta{FileName: "abort.txt", FileSize: 8})
		require.NoError(t, err)
		key := initRes.ObjectKey

		require.NoError(t, svc.Abort(ctx, initRes.UploadID, initRes.Token))
		require.Equal(t, model.BlobAborted, blobRowOf(t, g, key).State, "Abort 必须转 aborted")
	})
}
