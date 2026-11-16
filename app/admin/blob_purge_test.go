package admin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/dao"
	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/gorm"
)

// admin「彻底删除」全链负对照（core#172 第二片）：
// 软删 → 回收站 → 彻底删除 ⇒ 行删 + 引用释放（同事务）+ 真对象消失 +
// 账本推进到 purged 终态；软删/共享对象两条负对照。

// newBlobPurgeTestEnv 真实 local 存储 + 账本四表的集成环境：
// PurgeFiles 与 ReclaimBlobs 共用同一存储/库，跑完整状态机链条。
func newBlobPurgeTestEnv(t *testing.T) (string, *Service, *Janitor, *gorm.DB) {
	t.Helper()
	root, svcStorage, g := newJanitorTestEnv(t)
	require.NoError(t, g.AutoMigrate(
		&model.Blob{}, &model.FileReference{}, &model.SystemConfigRecord{},
	))
	svc := NewService(g)
	svc.SetStorage(svcStorage)
	j := NewJanitor(g, svcStorage, 90)
	return root, svc, j, g
}

// writePurgeObj 写物理对象。
func writePurgeObj(t *testing.T, root, rel string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
	require.NoError(t, os.WriteFile(abs, []byte("payload"), 0o644))
}

// 正例全链：软删（对象不动、引用不动）→ 彻底删除（对象消失、引用释放、
// blob 转 trashed）→ ReclaimBlobs 过宽限（账本推进 purged 终态）。
func TestPurgeFiles_FullLifecycleChain(t *testing.T) {
	root, svc, j, g := newBlobPurgeTestEnv(t)
	ctx := context.Background()
	const rel = "uploads/2026/11/16/chain.bin"
	writePurgeObj(t, root, rel)
	abs := filepath.Join(root, filepath.FromSlash(rel))

	// 存量形状：分享 + 账本（回填后的稳态）
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
		Code: "CHAIN01", FilePath: rel, Text: "chain.bin", Size: 7, ExpiredCount: -1,
	}))
	fc, err := svc.fileCodeRepo.GetByCode(ctx, "CHAIN01")
	require.NoError(t, err)
	blobRepo := dao.NewBlobRepository(g)
	blobID, err := blobRepo.EnsureBlobWithRef(ctx, rel, 7, "", fc.ID)
	require.NoError(t, err)

	// ① 软删：对象不动、引用不动（负对照前半）
	require.NoError(t, svc.fileCodeRepo.DeleteByIDTx(ctx, fc.ID))
	assert.FileExists(t, abs, "软删后对象必须保留")
	ref := mustRef(t, g, fc.ID)
	assert.Equal(t, model.RefActive, ref.State, "软删不得释放引用")

	// ② 彻底删除：真对象消失 + 引用释放 + blob 转 trashed
	purged, err := svc.PurgeFiles(ctx, []uint{fc.ID})
	require.NoError(t, err)
	require.Equal(t, int64(1), purged)
	assert.NoFileExists(t, abs, "硬删后物理对象必须真消失")
	ref = mustRef(t, g, fc.ID)
	assert.Equal(t, model.RefReleased, ref.State, "硬删必须释放引用")
	b := mustBlob(t, g, blobID)
	assert.Equal(t, model.BlobTrashed, b.State, "引用归零 ⇒ trashed（宽限期起点）")

	// ③ janitor 过宽限：账本推进 purged 终态（对象已删 ⇒ 幂等收敛）
	n, err := j.ReclaimBlobs(ctx, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "归零过宽限应推进 1 条")
	b = mustBlob(t, g, blobID)
	assert.Equal(t, model.BlobPurged, b.State, "状态机必须走到 purged 终态")
	require.NotNil(t, b.PurgedAt)
}

// 负对照①：软删（回收站语义）绝不释放账本引用、blob 保持 active。
//
// 口径注记：本片断言的是**账本不变量**（软删不释放——回收站行仍占对象）。
// 物理对象层面用纯 DB 软删（DeleteByIDTx，用户面回收站入口）而非
// BatchDeleteFiles——后者既有语义就是「软删 + best-effort 删对象
// （共享保护，2026-10-06 远端孤儿修复）」，不是本片的改动面。
func TestSoftDelete_KeepsLedgerRefActive(t *testing.T) {
	root, svc, _, g := newBlobPurgeTestEnv(t)
	ctx := context.Background()
	const rel = "uploads/2026/11/16/soft.bin"
	writePurgeObj(t, root, rel)
	abs := filepath.Join(root, filepath.FromSlash(rel))

	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
		Code: "SOFT001", FilePath: rel, Text: "soft.bin", Size: 7, ExpiredCount: -1,
	}))
	fc, err := svc.fileCodeRepo.GetByCode(ctx, "SOFT001")
	require.NoError(t, err)
	blobRepo := dao.NewBlobRepository(g)
	blobID, err := blobRepo.EnsureBlobWithRef(ctx, rel, 7, "", fc.ID)
	require.NoError(t, err)

	require.NoError(t, svc.fileCodeRepo.DeleteByIDTx(ctx, fc.ID)) // 回收站软删

	assert.FileExists(t, abs, "回收站软删后对象必须保留（恢复后对象必须在）")
	ref := mustRef(t, g, fc.ID)
	assert.Equal(t, model.RefActive, ref.State, "软删不得释放引用")
	b := mustBlob(t, g, blobID)
	assert.Equal(t, model.BlobActive, b.State, "软删后 blob 必须保持 active")
}

// 负对照②：共享对象（两分享同路径）硬删其一 ⇒ 对象保留、另一引用仍
// active、blob 不进回收（引用未归零不误删）。
func TestPurgeFiles_SharedObjectNotDeleted(t *testing.T) {
	root, svc, j, g := newBlobPurgeTestEnv(t)
	ctx := context.Background()
	const rel = "uploads/2026/11/16/shared.bin"
	writePurgeObj(t, root, rel)
	abs := filepath.Join(root, filepath.FromSlash(rel))

	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
		Code: "SHRD001", FilePath: rel, Text: "a.bin", Size: 7, ExpiredCount: -1,
	}))
	fcA, err := svc.fileCodeRepo.GetByCode(ctx, "SHRD001")
	require.NoError(t, err)
	require.NoError(t, svc.fileCodeRepo.Create(ctx, &model.FileCode{
		Code: "SHRD002", FilePath: rel, Text: "b.bin", Size: 7, ExpiredCount: -1,
	}))
	fcB, err := svc.fileCodeRepo.GetByCode(ctx, "SHRD002")
	require.NoError(t, err)

	blobRepo := dao.NewBlobRepository(g)
	blobID, err := blobRepo.EnsureBlobWithRef(ctx, rel, 7, "", fcA.ID)
	require.NoError(t, err)
	_, err = blobRepo.EnsureBlobWithRef(ctx, rel, 7, "", fcB.ID)
	require.NoError(t, err)

	// 硬删 A（先软删再 purge，模拟回收站操作）
	require.NoError(t, svc.fileCodeRepo.DeleteByIDTx(ctx, fcA.ID))
	_, err = svc.PurgeFiles(ctx, []uint{fcA.ID})
	require.NoError(t, err)

	assert.FileExists(t, abs, "共享对象不得被删（B 仍引用）")
	refA := mustRef(t, g, fcA.ID)
	refB := mustRef(t, g, fcB.ID)
	assert.Equal(t, model.RefReleased, refA.State, "A 的引用应释放")
	assert.Equal(t, model.RefActive, refB.State, "B 的引用必须仍 active")
	b := mustBlob(t, g, blobID)
	assert.Equal(t, model.BlobActive, b.State, "引用未归零 ⇒ blob 不得进回收")

	// ReclaimBlobs 双守卫：路径引用仍在 ⇒ 不删
	n, err := j.ReclaimBlobs(ctx, 0)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "引用未归零不得回收")
	assert.FileExists(t, abs, "共享对象必须原样保留")
}

// mustRef / mustBlob 断言辅助（直接查测试库）。
func mustRef(t *testing.T, g *gorm.DB, shareID uint) model.FileReference {
	t.Helper()
	var ref model.FileReference
	require.NoError(t, g.Unscoped().
		Where("file_code_id = ?", shareID).First(&ref).Error)
	return ref
}

func mustBlob(t *testing.T, g *gorm.DB, id uint) model.Blob {
	t.Helper()
	var b model.Blob
	require.NoError(t, g.First(&b, id).Error)
	return b
}
