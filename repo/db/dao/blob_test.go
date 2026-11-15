package dao

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// newBlobTestDB blobs/file_references 测试库（单连接，同 governance 口径）。
func newBlobTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.Blob{}, &model.FileReference{}, &model.FileCode{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return g
}

// seedShareRow 造一条分享行（账本 file_code_id 指向真实分享）。
func seedShareRow(t *testing.T, g *gorm.DB, code string) uint {
	t.Helper()
	fc := &model.FileCode{Code: code, Text: code}
	require.NoError(t, g.Create(fc).Error)
	return fc.ID
}

// reloadBlob 干净重读（gorm First 复用带 ID 的结构体 WHERE 会叠加）。
func reloadBlob(t *testing.T, g *gorm.DB, id uint) model.Blob {
	t.Helper()
	var b model.Blob
	require.NoError(t, g.First(&b, id).Error)
	return b
}

// 双写基本面：建 blob(active) + ref(active)；重复调用幂等（不产生第二行）。
func TestEnsureBlobWithRef_CreateAndIdempotent(t *testing.T) {
	g := newBlobTestDB(t)
	repo := NewBlobRepository(g)
	ctx := context.Background()
	shareID := seedShareRow(t, g, "BLB00001")

	blobID, err := repo.EnsureBlobWithRef(ctx, "uploads/2026/11/16/a.bin", 10, "hash-a", shareID)
	require.NoError(t, err)
	require.NotZero(t, blobID)

	// 幂等：同参数再来一次 —— 同 blob、同 ref，行数不涨
	blobID2, err := repo.EnsureBlobWithRef(ctx, "uploads/2026/11/16/a.bin", 10, "hash-a", shareID)
	require.NoError(t, err)
	assert.Equal(t, blobID, blobID2, "同 object_key 必须复用同一 blob 行")

	var blobs int64
	require.NoError(t, g.Model(&model.Blob{}).Count(&blobs).Error)
	assert.Equal(t, int64(1), blobs, "幂等不得建第二个 blob")
	var refs int64
	require.NoError(t, g.Model(&model.FileReference{}).Count(&refs).Error)
	assert.Equal(t, int64(1), refs, "幂等不得建第二个引用")

	// 状态断言
	var b model.Blob
	require.NoError(t, g.First(&b, blobID).Error)
	assert.Equal(t, model.BlobActive, b.State)
	var rf model.FileReference
	require.NoError(t, g.First(&rf).Error)
	assert.Equal(t, model.RefActive, rf.State)
}

// 参数守卫：空 objectKey / 0 shareID 拒绝。
func TestEnsureBlobWithRef_ValidatesParams(t *testing.T) {
	g := newBlobTestDB(t)
	repo := NewBlobRepository(g)
	ctx := context.Background()

	_, err := repo.EnsureBlobWithRef(ctx, "", 1, "h", 1)
	assert.Error(t, err, "空 objectKey 必须拒绝")
	_, err = repo.EnsureBlobWithRef(ctx, "uploads/x", 1, "h", 0)
	assert.Error(t, err, "fileCodeID=0 必须拒绝")
}

// trashed 行被重新引用 ⇒ 沿合法链推进回 active（宽限期内恢复）。
func TestEnsureBlobWithRef_RevivesTrashedBlob(t *testing.T) {
	g := newBlobTestDB(t)
	repo := NewBlobRepository(g)
	ctx := context.Background()
	shareA := seedShareRow(t, g, "BLB00002")
	shareB := seedShareRow(t, g, "BLB00003")

	blobID, err := repo.EnsureBlobWithRef(ctx, "uploads/revive.bin", 5, "h", shareA)
	require.NoError(t, err)
	_, err = repo.ReleaseByShare(ctx, shareA)
	require.NoError(t, err)

	var b model.Blob
	require.NoError(t, g.First(&b, blobID).Error)
	require.Equal(t, model.BlobTrashed, b.State, "单引用释放后应进 trashed")

	// 新分享重新引用同对象 ⇒ 恢复 active
	_, err = repo.EnsureBlobWithRef(ctx, "uploads/revive.bin", 5, "h", shareB)
	require.NoError(t, err)
	require.NoError(t, g.First(&b, blobID).Error)
	assert.Equal(t, model.BlobActive, b.State, "trashed → active 应沿合法链恢复")
}

// 终态行不可复用：purged/aborted 拒绝（ErrBlobNotReusable），不静默改账。
func TestEnsureBlobWithRef_RefusesTerminalStates(t *testing.T) {
	for _, st := range []string{model.BlobPurged, model.BlobAborted} {
		g := newBlobTestDB(t)
		repo := NewBlobRepository(g)
		ctx := context.Background()
		shareID := seedShareRow(t, g, "BLB00004")

		require.NoError(t, g.Create(&model.Blob{
			ObjectKey: "uploads/terminal.bin", State: st,
		}).Error)
		shareID2 := seedShareRow(t, g, "BLB00005")

		_, err := repo.EnsureBlobWithRef(ctx, "uploads/terminal.bin", 1, "h", shareID2)
		assert.Errorf(t, err, "状态 %s 应拒绝复用", st)
		assert.ErrorIs(t, err, ErrBlobNotReusable)
		_ = shareID
	}
}

// 释放基本面：单引用分享硬删 ⇒ 引用 released + blob 归零进 trashed（记 trashed_at）。
func TestReleaseByShare_SingleRefReclaimsToTrashed(t *testing.T) {
	g := newBlobTestDB(t)
	repo := NewBlobRepository(g)
	ctx := context.Background()
	shareID := seedShareRow(t, g, "BLB00006")

	blobID, err := repo.EnsureBlobWithRef(ctx, "uploads/single.bin", 8, "h", shareID)
	require.NoError(t, err)

	n, err := repo.ReleaseByShare(ctx, shareID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "应释放 1 条引用")

	var b model.Blob
	require.NoError(t, g.First(&b, blobID).Error)
	assert.Equal(t, model.BlobTrashed, b.State, "归零应进 trashed")
	require.NotNil(t, b.TrashedAt, "trashed_at = 宽限期起点，必须落")

	var rf model.FileReference
	require.NoError(t, g.First(&rf).Error)
	assert.Equal(t, model.RefReleased, rf.State)
	require.NotNil(t, rf.ReleasedAt)

	// 幂等：再释放一次 = 0，不报错
	n, err = repo.ReleaseByShare(ctx, shareID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "重复释放应幂等")
}

// 负对照①：共享对象（两分享引用同一 blob）——释放其一，引用未归零 ⇒ 不进回收。
func TestReleaseByShare_SharedObjectStaysActive(t *testing.T) {
	g := newBlobTestDB(t)
	repo := NewBlobRepository(g)
	ctx := context.Background()
	shareA := seedShareRow(t, g, "BLB00007")
	shareB := seedShareRow(t, g, "BLB00008")

	blobID, err := repo.EnsureBlobWithRef(ctx, "uploads/shared.bin", 3, "h", shareA)
	require.NoError(t, err)
	_, err = repo.EnsureBlobWithRef(ctx, "uploads/shared.bin", 3, "h", shareB)
	require.NoError(t, err)

	// 硬删 A
	n, err := repo.ReleaseByShare(ctx, shareA)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	var b model.Blob
	require.NoError(t, g.First(&b, blobID).Error)
	assert.Equal(t, model.BlobActive, b.State,
		"负对照①：引用未归零（B 仍引用）不得进回收")

	alive, err := repo.CountActiveRefs(ctx, blobID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), alive, "B 的引用应仍 active")

	// 硬删 B ⇒ 归零
	_, err = repo.ReleaseByShare(ctx, shareB)
	require.NoError(t, err)
	require.NoError(t, g.First(&b, blobID).Error)
	assert.Equal(t, model.BlobTrashed, b.State, "全部释放后才进 trashed")
}

// 回收候选：过宽限的 trashed + purge（重试）在列；active、未过宽限的 trashed 不在列。
func TestListReclaimable_GraceAndRetry(t *testing.T) {
	g := newBlobTestDB(t)
	repo := NewBlobRepository(g)
	ctx := context.Background()

	old := time.Now().Add(-48 * time.Hour)
	fresh := time.Now()

	rows := []model.Blob{
		{ObjectKey: "k-expired", State: model.BlobTrashed, TrashedAt: &old}, // 过宽限 ⇒ 候选
		{ObjectKey: "k-fresh", State: model.BlobTrashed, TrashedAt: &fresh}, // 未过宽限 ⇒ 不候选
		{ObjectKey: "k-active", State: model.BlobActive},                    // 有引用 ⇒ 不候选
		{ObjectKey: "k-purge", State: model.BlobPurge},                      // 上轮删失败重试 ⇒ 候选
		{ObjectKey: "k-purged", State: model.BlobPurged},                    // 终态 ⇒ 不候选
		{ObjectKey: "k-notrash", State: model.BlobTrashed},                  // trashed_at 空 ⇒ 不候选（脏数据保守）
	}
	for i := range rows {
		require.NoError(t, g.Create(&rows[i]).Error)
	}

	cutoff := time.Now().Add(-24 * time.Hour)
	got, err := repo.ListReclaimable(ctx, cutoff)
	require.NoError(t, err)

	keys := make(map[string]bool, len(got))
	for _, b := range got {
		keys[b.ObjectKey] = true
	}
	assert.True(t, keys["k-expired"], "过宽限 trashed 应在候选")
	assert.True(t, keys["k-purge"], "purge 重试应在候选")
	assert.False(t, keys["k-fresh"], "未过宽限不得候选")
	assert.False(t, keys["k-active"], "active 不得候选")
	assert.False(t, keys["k-purged"], "终态不得候选")
	assert.False(t, keys["k-notrash"], "trashed_at 为空不得候选")
	assert.Len(t, got, 2, "候选应恰好 2 个")
}

// CAS 转移：合法转移成功且落时间戳；非法转移在 Go 层拒绝；from 不符 ⇒ false 不改账。
func TestTransitionBlob_LegalIllegalAndCAS(t *testing.T) {
	g := newBlobTestDB(t)
	repo := NewBlobRepository(g)
	ctx := context.Background()

	mk := func(key, state string) uint {
		b := model.Blob{ObjectKey: key, State: state}
		require.NoError(t, g.Create(&b).Error)
		return b.ID
	}

	// 合法：active → trashed，记 trashed_at
	id := mk("k1", model.BlobActive)
	ok, err := repo.TransitionBlob(ctx, id, model.BlobActive, model.BlobTrashed)
	require.NoError(t, err)
	assert.True(t, ok)
	b := reloadBlob(t, g, id)
	assert.Equal(t, model.BlobTrashed, b.State)
	require.NotNil(t, b.TrashedAt)

	// 非法：active → purged（跳态）⇒ Go 层拒绝，账本不动
	id2 := mk("k2", model.BlobActive)
	_, err = repo.TransitionBlob(ctx, id2, model.BlobActive, model.BlobPurged)
	assert.Error(t, err, "跳态转移必须拒绝")
	b = reloadBlob(t, g, id2)
	assert.Equal(t, model.BlobActive, b.State, "非法转移不得改账")

	// CAS：from 不符 ⇒ false（不改账、不报错）
	id3 := mk("k3", model.BlobActive)
	ok, err = repo.TransitionBlob(ctx, id3, model.BlobTrashed, model.BlobPurge)
	require.NoError(t, err)
	assert.False(t, ok, "from 不符应返回 false")
	b = reloadBlob(t, g, id3)
	assert.Equal(t, model.BlobActive, b.State)

	// purged 落 purged_at
	id4 := mk("k4", model.BlobPurge)
	ok, err = repo.TransitionBlob(ctx, id4, model.BlobPurge, model.BlobPurged)
	require.NoError(t, err)
	assert.True(t, ok)
	b = reloadBlob(t, g, id4)
	require.NotNil(t, b.PurgedAt)
}
