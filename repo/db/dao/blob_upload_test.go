package dao

import (
	"context"
	"errors"
	"testing"

	"github.com/pigeonbox/core/pkg/metrics"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// blobState 直读现态（测试断言用）。
func blobState(t *testing.T, g *gorm.DB, key string) string {
	t.Helper()
	var b model.Blob
	require.NoError(t, g.First(&b, "object_key = ?", key).Error)
	return b.State
}

// 完整生产链（presign 入口）：pending 落行 → uploading → committed，逐跳合法、
// 读数与指标同步。
func TestEnsureUploadBlob_PendingThroughCommitted(t *testing.T) {
	g := newBlobTestDB(t)
	repo := NewBlobRepository(g)
	ctx := context.Background()
	const key = "uploads/2026/11/16/p3-chain.bin"

	// 全局计数器用增量断言（用例执行顺序不设防——别的用例也可能记同标签）
	mCreated0 := metrics.BlobUploadTransitionsValue("none", model.BlobPending, UploadTxCreated)
	mPU0 := metrics.BlobUploadTransitionsValue(model.BlobPending, model.BlobUploading, UploadTxAdvanced)
	mUC0 := metrics.BlobUploadTransitionsValue(model.BlobUploading, model.BlobCommitted, UploadTxAdvanced)

	// 1. presign Init 入口：pending 落行
	from, outcome, err := repo.EnsureUploadBlob(ctx, key, 1024, "hash-p3", model.BlobPending)
	require.NoError(t, err)
	assert.Equal(t, "none", from)
	assert.Equal(t, UploadTxCreated, outcome)
	assert.Equal(t, model.BlobPending, blobState(t, g, key))

	// 2. PUT 开始：pending → uploading
	from, outcome, err = repo.AdvanceUploadBlob(ctx, key, model.BlobUploading)
	require.NoError(t, err)
	assert.Equal(t, model.BlobPending, from)
	assert.Equal(t, UploadTxAdvanced, outcome)
	assert.Equal(t, model.BlobUploading, blobState(t, g, key))

	// 3. PUT 完成：uploading → committed（size 只填空回填：已有 1024 不被覆盖）
	from, outcome, err = repo.AdvanceUploadBlob(ctx, key, model.BlobCommitted)
	require.NoError(t, err)
	assert.Equal(t, model.BlobUploading, from)
	assert.Equal(t, UploadTxAdvanced, outcome)
	assert.Equal(t, model.BlobCommitted, blobState(t, g, key))

	var b model.Blob
	require.NoError(t, g.First(&b, "object_key = ?", key).Error)
	assert.Equal(t, int64(1024), b.Size, "申报大小应落账")
	assert.Equal(t, "hash-p3", b.FileHash)

	// 指标：三笔各就各位（增量各 +1）
	assert.Equal(t, mCreated0+1, metrics.BlobUploadTransitionsValue("none", model.BlobPending, UploadTxCreated))
	assert.Equal(t, mPU0+1, metrics.BlobUploadTransitionsValue(model.BlobPending, model.BlobUploading, UploadTxAdvanced))
	assert.Equal(t, mUC0+1, metrics.BlobUploadTransitionsValue(model.BlobUploading, model.BlobCommitted, UploadTxAdvanced))
}

// 分片合并入口：行不存在时以 committed 直接入账（字节已落盘、尚无分享引用）。
func TestEnsureUploadBlob_MergeCreatesCommitted(t *testing.T) {
	g := newBlobTestDB(t)
	repo := NewBlobRepository(g)
	ctx := context.Background()
	const key = "uploads/2026/11/16/p3-merged.bin"

	from, outcome, err := repo.EnsureUploadBlob(ctx, key, 0, "", model.BlobCommitted)
	require.NoError(t, err)
	assert.Equal(t, "none", from)
	assert.Equal(t, UploadTxCreated, outcome)
	assert.Equal(t, model.BlobCommitted, blobState(t, g, key))

	// 幂等：再落一次（重复合并/重放）不建第二行、不改状态
	from, outcome, err = repo.EnsureUploadBlob(ctx, key, 0, "", model.BlobCommitted)
	require.NoError(t, err)
	assert.Equal(t, model.BlobCommitted, from)
	assert.Equal(t, UploadTxNoop, outcome)
	var n int64
	require.NoError(t, g.Model(&model.Blob{}).Where("object_key = ?", key).Count(&n).Error)
	assert.Equal(t, int64(1), n, "幂等不得建第二个 blob 行")
}

// 负对照①：回退一律拒绝——active（分享已发布）不许退回 committed/uploading。
func TestEnsureUploadBlob_RejectsBackward(t *testing.T) {
	g := newBlobTestDB(t)
	repo := NewBlobRepository(g)
	ctx := context.Background()
	const key = "uploads/2026/11/16/p3-back.bin"
	shareID := seedShareRow(t, g, "P3BACK01")

	// 建即 active（分享创建入口语义）
	_, err := repo.EnsureBlobWithRef(ctx, key, 10, "h", shareID)
	require.NoError(t, err)

	for _, target := range []string{model.BlobCommitted, model.BlobUploading, model.BlobPending} {
		from, outcome, err := repo.EnsureUploadBlob(ctx, key, 10, "h", target)
		require.Error(t, err, "active → %s 必须被拒绝", target)
		assert.ErrorIs(t, err, ErrBlobNotReusable)
		assert.Equal(t, UploadTxRejected, outcome, "outcome 必须记 rejected（指标负对照）")
		assert.Equal(t, model.BlobActive, from)
		assert.Equal(t, model.BlobActive, blobState(t, g, key), "拒绝后状态不得被改写")
	}
	// Advance 同口径
	_, outcome, err := repo.AdvanceUploadBlob(ctx, key, model.BlobCommitted)
	require.ErrorIs(t, err, ErrBlobNotReusable)
	assert.Equal(t, UploadTxRejected, outcome)
}

// 负对照②：aborted 终态不复活——pending/uploading/committed 可入 aborted，
// aborted 回任何上传段状态一律拒绝（对象已判废，复活会指向不存在的字节）。
func TestEnsureUploadBlob_AbortTerminal(t *testing.T) {
	g := newBlobTestDB(t)
	repo := NewBlobRepository(g)
	ctx := context.Background()
	const key = "uploads/2026/11/16/p3-abort.bin"

	_, _, err := repo.EnsureUploadBlob(ctx, key, 5, "", model.BlobPending)
	require.NoError(t, err)

	// pending → aborted：合法单跳
	from, outcome, err := repo.AdvanceUploadBlob(ctx, key, model.BlobAborted)
	require.NoError(t, err)
	assert.Equal(t, model.BlobPending, from)
	assert.Equal(t, UploadTxAdvanced, outcome)
	assert.Equal(t, model.BlobAborted, blobState(t, g, key))

	// aborted → uploading/committed：终态复活拒绝
	for _, target := range []string{model.BlobUploading, model.BlobCommitted, model.BlobPending} {
		_, outcome, err := repo.AdvanceUploadBlob(ctx, key, target)
		require.ErrorIs(t, err, ErrBlobNotReusable, "aborted → %s 必须拒绝", target)
		assert.Equal(t, UploadTxRejected, outcome)
	}
	// aborted → aborted：幂等 noop
	_, outcome, err = repo.AdvanceUploadBlob(ctx, key, model.BlobAborted)
	require.NoError(t, err)
	assert.Equal(t, UploadTxNoop, outcome)
	assert.Equal(t, model.BlobAborted, blobState(t, g, key))
}

// 行不存在时 Advance ⇒ noop（会话从未落账：不建行、不报错，兜底在 EnsureBlobWithRef）。
func TestAdvanceUploadBlob_MissingRowNoop(t *testing.T) {
	g := newBlobTestDB(t)
	repo := NewBlobRepository(g)
	ctx := context.Background()

	from, outcome, err := repo.AdvanceUploadBlob(ctx, "uploads/2026/11/16/never.bin", model.BlobUploading)
	require.NoError(t, err)
	assert.Equal(t, "none", from)
	assert.Equal(t, UploadTxNoop, outcome)
	var n int64
	require.NoError(t, g.Model(&model.Blob{}).Count(&n).Error)
	assert.Equal(t, int64(0), n, "Advance 不得凭空建行")
}

// 一步跨多跳：pending 直接 ensure 到 committed ⇒ 逐跳走完（uploading 中转），
// 结果 advanced 且落点正确。
func TestEnsureUploadBlob_WalksMultiHop(t *testing.T) {
	g := newBlobTestDB(t)
	repo := NewBlobRepository(g)
	ctx := context.Background()
	const key = "uploads/2026/11/16/p3-walk.bin"

	_, _, err := repo.EnsureUploadBlob(ctx, key, 7, "h7", model.BlobPending)
	require.NoError(t, err)

	from, outcome, err := repo.EnsureUploadBlob(ctx, key, 7, "h7", model.BlobCommitted)
	require.NoError(t, err)
	assert.Equal(t, model.BlobPending, from)
	assert.Equal(t, UploadTxAdvanced, outcome)
	assert.Equal(t, model.BlobCommitted, blobState(t, g, key))
}

// 参数守卫：空 key / 非法建行目标（aborted、active）拒绝且不落行。
func TestEnsureUploadBlob_ValidatesParams(t *testing.T) {
	g := newBlobTestDB(t)
	repo := NewBlobRepository(g)
	ctx := context.Background()

	_, outcome, err := repo.EnsureUploadBlob(ctx, "", 1, "", model.BlobPending)
	require.Error(t, err)
	assert.Equal(t, UploadTxError, outcome)

	_, outcome, err = repo.EnsureUploadBlob(ctx, "k1", 1, "", model.BlobAborted)
	require.Error(t, err)
	assert.Equal(t, UploadTxError, outcome)

	_, outcome, err = repo.EnsureUploadBlob(ctx, "k2", 1, "", model.BlobActive)
	require.Error(t, err, "active 不是上传段建行目标（挂引用走 EnsureBlobWithRef）")
	assert.Equal(t, UploadTxError, outcome)

	_, outcome, err = repo.AdvanceUploadBlob(ctx, "k3", "nonsense")
	require.Error(t, err)
	assert.Equal(t, UploadTxError, outcome)

	var n int64
	require.NoError(t, g.Model(&model.Blob{}).Count(&n).Error)
	assert.Equal(t, int64(0), n, "参数拒绝不得落行")
}

// 与首片 EnsureBlobWithRef 的衔接：committed 行被分享创建沿 CanReach 推进到
// active 并挂引用（第三片的落点 = 首片的入口）。
func TestEnsureUploadBlob_HandoffToEnsureBlobWithRef(t *testing.T) {
	g := newBlobTestDB(t)
	repo := NewBlobRepository(g)
	ctx := context.Background()
	const key = "uploads/2026/11/16/p3-handoff.bin"
	shareID := seedShareRow(t, g, "P3HAND01")

	_, _, err := repo.EnsureUploadBlob(ctx, key, 42, "h42", model.BlobCommitted)
	require.NoError(t, err)

	blobID, err := repo.EnsureBlobWithRef(ctx, key, 42, "h42", shareID)
	require.NoError(t, err)
	assert.Equal(t, model.BlobActive, blobState(t, g, key), "committed → active 必须可达")

	// 引用挂上、size 保留
	var ref model.FileReference
	require.NoError(t, g.First(&ref, "blob_id = ? AND file_code_id = ?", blobID, shareID).Error)
	assert.Equal(t, model.RefActive, ref.State)
	var b model.Blob
	require.NoError(t, g.First(&b, blobID).Error)
	assert.Equal(t, int64(42), b.Size)
}

// rejected 归类不与 db error 混淆：状态机拒绝回 ErrBlobNotReusable 本身。
func TestEnsureUploadBlob_RejectedIsSemanticError(t *testing.T) {
	g := newBlobTestDB(t)
	repo := NewBlobRepository(g)
	ctx := context.Background()
	const key = "uploads/2026/11/16/p3-err.bin"
	shareID := seedShareRow(t, g, "P3ERR001")
	_, err := repo.EnsureBlobWithRef(ctx, key, 1, "h", shareID)
	require.NoError(t, err)

	_, outcome, err := repo.EnsureUploadBlob(ctx, key, 1, "h", model.BlobUploading)
	require.True(t, errors.Is(err, ErrBlobNotReusable))
	assert.Equal(t, UploadTxRejected, outcome)
}
