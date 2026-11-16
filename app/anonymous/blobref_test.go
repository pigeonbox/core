// 匿名占位通道账本挂接守卫（core#172 拆分项 3：匿名直建面 Reference 化）。
//
// ## 为什么有这道门禁
//
// 匿名通道直建/回填 file_codes 行、不经 share.CreateShare ⇒ 首片挂在分享域的
// attachBlobRef 覆盖不到；漏挂账的后果是「释放/回收时账本侧查不到引用」
// （双守卫的账本侧失明），且没有任何机制要求新直建面补挂。
//
// ⇒ 本守卫两层机械判据：
//
//	① 行为级：直建/回填成功 ⇒ blobs(active) + file_references(active) 必须落账；
//	  占位期（无对象）必须零账本行；
//	② 源码级：app/anonymous 生产文件里任何含 fileCodeRepo.Create( 或
//	  "file_path": 回填的函数必须调用 attachBlobRef——新增直建面漏挂 ⇒ 红
//	  （并要求至少扫到 2 个写面，防判据静默失效）。
//
// 负对照（已实跑）：摘掉 CreateAnonymousShare 的 attachBlobRef ⇒ ①② 红；
// 摘掉 BindFilePath 的 ⇒ ①② 红；还原后全绿。
package anonymous

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// newBlobRefTestService 复用 newTestService 骨架，另建账本两表
// （骨架 AutoMigrate 清单不含新表，同 app/share/blobref_test.go 的处理）。
func newBlobRefTestService(t *testing.T) (*Service, *gorm.DB) {
	t.Helper()
	svc, _, g := newTestService(t)
	require.NoError(t, g.AutoMigrate(&model.Blob{}, &model.FileReference{}))
	return svc, g
}

func blobRows(t *testing.T, g *gorm.DB) []model.Blob {
	t.Helper()
	var rows []model.Blob
	require.NoError(t, g.Find(&rows).Error)
	return rows
}

func refRows(t *testing.T, g *gorm.DB) []model.FileReference {
	t.Helper()
	var rows []model.FileReference
	require.NoError(t, g.Find(&rows).Error)
	return rows
}

// 行为级①：直建行带对象 ⇒ 落账（blob active + 引用挂在新建行上）。
func TestCreateAnonymousShare_WritesBlobAndRef(t *testing.T) {
	svc, g := newBlobRefTestService(t)
	ctx := context.Background()

	_, err := svc.CreateAnonymousShare(ctx, AnonymousShareParams{
		FilePath: "uploads/anon/a.bin", FileName: "a.bin", FileSize: 10,
		MaxPickupCount: -1,
	})
	require.NoError(t, err)

	var fc model.FileCode
	require.NoError(t, g.First(&fc).Error)

	blobs := blobRows(t, g)
	require.Len(t, blobs, 1, "匿名直建面必须落账")
	assert.Equal(t, "uploads/anon/a.bin", blobs[0].ObjectKey)
	assert.Equal(t, model.BlobActive, blobs[0].State, "写路径建即 active（同 EnsureBlobWithRef 口径）")
	assert.Equal(t, int64(10), blobs[0].Size)

	refs := refRows(t, g)
	require.Len(t, refs, 1, "必须挂一条引用")
	assert.Equal(t, model.RefActive, refs[0].State)
	assert.Equal(t, blobs[0].ID, refs[0].BlobID)
	assert.Equal(t, fc.ID, refs[0].FileCodeID, "引用应挂在匿名直建行上")
}

// 行为级②：生产 /anonymous/generate 形态（FilePath="" 占位）⇒ 无对象不落账。
func TestCreateAnonymousShare_PlaceholderNoLedgerRow(t *testing.T) {
	svc, g := newBlobRefTestService(t)
	ctx := context.Background()

	_, err := svc.CreateAnonymousShare(ctx, AnonymousShareParams{
		FilePath: "", FileName: "ph.bin", FileSize: 3,
		MaxPickupCount: -1,
	})
	require.NoError(t, err)

	assert.Empty(t, blobRows(t, g), "占位期无物理对象，不得落 blob")
	assert.Empty(t, refRows(t, g), "占位期不得落引用")
}

// 行为级③：占位回填（presign 取件码绑定最后一公里）⇒ 回填成功必须补挂账。
func TestBindFilePath_BackfillsLedgerRef(t *testing.T) {
	svc, g := newBlobRefTestService(t)
	ctx := context.Background()

	_, err := svc.CreateAnonymousShare(ctx, AnonymousShareParams{
		FilePath: "", FileName: "bind.bin", FileSize: 0,
		MaxPickupCount: -1,
	})
	require.NoError(t, err)
	var fc model.FileCode
	require.NoError(t, g.First(&fc).Error)
	require.Empty(t, blobRows(t, g), "回填前零账本行")

	require.NoError(t, svc.BindFilePath(ctx, fc.Code, "uploads/anon/bind.bin", 42, ""))

	blobs := blobRows(t, g)
	require.Len(t, blobs, 1, "回填后必须落账")
	assert.Equal(t, "uploads/anon/bind.bin", blobs[0].ObjectKey)
	assert.Equal(t, model.BlobActive, blobs[0].State, "#208 会话段的 committed 应被推进到 active")
	assert.Equal(t, int64(42), blobs[0].Size, "size 以回填的实际大小为准")

	refs := refRows(t, g)
	require.Len(t, refs, 1)
	assert.Equal(t, model.RefActive, refs[0].State)
	assert.Equal(t, fc.ID, refs[0].FileCodeID, "引用应挂在占位直建行上")

	// 幂等护栏：重复绑定不得翻倍
	require.NoError(t, svc.BindFilePath(ctx, fc.Code, "uploads/anon/bind.bin", 42, ""))
	assert.Len(t, blobRows(t, g), 1, "重复绑定 blob 不得翻倍")
	assert.Len(t, refRows(t, g), 1, "重复绑定引用不得翻倍")
}

// 源码级守卫：直建/回填函数必须调用 attachBlobRef（新增面漏挂 ⇒ 红）。
func TestAnonymousDirectWriteFacesMustAttachLedger(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, rerr := os.ReadFile(name)
		require.NoError(t, rerr)
		for _, block := range splitFuncBlocks(string(data)) {
			writes := strings.Contains(block, "fileCodeRepo.Create(") || strings.Contains(block, "\"file_path\":")
			if !writes {
				continue
			}
			checked++
			if !strings.Contains(block, "attachBlobRef(") {
				t.Errorf("文件 %s 存在建行/回填 file_path 的函数但未调用 attachBlobRef——匿名直建面漏挂账（core#172 拆分项 3）", name)
			}
		}
	}
	// 判据自身失效防护：至少应扫到 CreateAnonymousShare 与 BindFilePath 两个写面。
	require.GreaterOrEqual(t, checked, 2,
		"守卫判据失效：只扫到 %d 个直建写面（应≥2：CreateAnonymousShare/BindFilePath）——源码形态变了请更新本判据", checked)
}

// splitFuncBlocks 按 "func " 定义切块（首块为包头/导入；块内含该函数全文）。
func splitFuncBlocks(src string) []string {
	parts := strings.Split(src, "func ")
	if len(parts) <= 1 {
		return nil
	}
	blocks := make([]string, 0, len(parts)-1)
	for _, p := range parts[1:] {
		blocks = append(blocks, "func "+p)
	}
	return blocks
}
