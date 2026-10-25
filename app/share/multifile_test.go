package share

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =====================================================================
// 多文件分享单测（P0 多文件）：创建/列表/子文件归属/删除联动/zip 名消毒
// =====================================================================

func multiEntries(n int, sizeEach int64) []StoredFileEntry {
	entries := make([]StoredFileEntry, 0, n)
	for i := 0; i < n; i++ {
		entries = append(entries, StoredFileEntry{
			RelPath:  "uploads/2026/10/03/uuid-" + string(rune('a'+i)) + ".txt",
			FileName: "文件" + string(rune('A'+i)) + ".txt",
			Size:     sizeEach,
		})
	}
	return entries
}

func TestCreateMultiFileShare(t *testing.T) {
	svc, st, usr, _ := newTestService(t)
	ctx := context.Background()

	resp, err := svc.CreateMultiFileShare(ctx, &MultiShareReq{
		Entries:      multiEntries(3, 100),
		UploadType:   "anonymous",
		OwnerIP:      "1.2.3.4",
		Channel:      "chunk",
		ExpiredCount: -1,
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	// 主表 Size = 各文件之和
	fc, err := svc.GetFileByCode(ctx, resp.Code)
	require.NoError(t, err)
	assert.Equal(t, int64(300), fc.Size)
	// legacy 字段与首个文件同步（秒传/管理端兼容）
	assert.Contains(t, fc.FilePath, "uuid-a.txt")
	assert.Equal(t, "文件A.txt", fc.Text)

	// 子表 3 行，ListShareFiles 返回 3 项
	items, err := svc.ListShareFiles(ctx, resp.Code)
	require.NoError(t, err)
	require.Len(t, items, 3)
	assert.Equal(t, "文件A.txt", items[0].Name)
	assert.Equal(t, int64(100), items[0].Size)

	// 用户统计未触发（匿名）
	assert.Equal(t, int64(0), usr.uploadsCalls)
	_ = st
}

func TestCreateMultiFileShare_UserStatsAndQuota(t *testing.T) {
	svc, _, usr, _ := newTestService(t)
	uid := uint(7)
	ctx := context.Background()

	resp, err := svc.CreateMultiFileShare(ctx, &MultiShareReq{
		Entries:    multiEntries(2, 500),
		UserID:     &uid,
		UploadType: "authenticated",
		OwnerIP:    "1.2.3.4",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), usr.uploadsCalls)
	assert.Equal(t, int64(1000), usr.storageDelta)
	_ = resp
}

func TestCreateMultiFileShare_EmptyAndTooMany(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()

	_, err := svc.CreateMultiFileShare(ctx, &MultiShareReq{})
	require.Error(t, err)

	entries := make([]StoredFileEntry, 101)
	for i := range entries {
		entries[i] = StoredFileEntry{RelPath: "x", FileName: "x", Size: 1}
	}
	_, err = svc.CreateMultiFileShare(ctx, &MultiShareReq{Entries: entries})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "上限")
}

func TestGetShareChild_Ownership(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()

	r1, err := svc.CreateMultiFileShare(ctx, &MultiShareReq{Entries: multiEntries(2, 10), OwnerIP: "1.1.1.1"})
	require.NoError(t, err)
	r2, err := svc.CreateMultiFileShare(ctx, &MultiShareReq{Entries: multiEntries(2, 10), OwnerIP: "2.2.2.2"})
	require.NoError(t, err)

	items1, err := svc.ListShareFiles(ctx, r1.Code)
	require.NoError(t, err)
	require.Len(t, items1, 2)

	// 归属内子文件 OK
	fc, child, err := svc.GetShareChild(ctx, r1.Code, items1[0].ID)
	require.NoError(t, err)
	require.NotNil(t, child)
	assert.Equal(t, r1.Code, fc.Code)

	// 跨分享子文件 ID 拒绝
	_, _, err = svc.GetShareChild(ctx, r2.Code, items1[0].ID)
	require.Error(t, err)

	// 不存在
	_, _, err = svc.GetShareChild(ctx, r1.Code, 99999)
	require.Error(t, err)
}

func TestDeleteFileByCode_RemovesChildren(t *testing.T) {
	svc, st, _, _ := newTestService(t)
	ctx := context.Background()
	uid := uint(9)

	resp, err := svc.CreateMultiFileShare(ctx, &MultiShareReq{
		Entries:    multiEntries(3, 100),
		UserID:     &uid,
		UploadType: "authenticated",
		OwnerIP:    "1.2.3.4",
	})
	require.NoError(t, err)

	require.NoError(t, svc.DeleteFileByCode(ctx, resp.Code, uid))

	// 子表软删：ListShareFiles 回退 legacy 主表（主记录已删 → 报错或空）
	fc, err := svc.GetFileByCode(ctx, resp.Code)
	require.Error(t, err, "主记录已删")
	_ = fc
	// 物理删除调用覆盖 3 个子文件 + 1 个主表路径
	assert.GreaterOrEqual(t, len(st.deletedPaths), 3)
}

func TestZipSafeName(t *testing.T) {
	// 去路径（Zip Slip 防护）
	assert.Equal(t, "a.txt", zipSafeName("../../etc/a.txt", nil))
	assert.Equal(t, "a.txt", zipSafeName(`..\..\a.txt`, nil))
	// 控制字符剔除
	assert.Equal(t, "ab.txt", zipSafeName("a\x00b.txt", nil))
	// 空名兜底
	assert.Equal(t, "file", zipSafeName("", nil))
	// 重名追加序号
	used := []string{zipSafeName("a.txt", nil)}
	assert.Equal(t, "a(1).txt", zipSafeName("a.txt", used))
}

// ===== zip 流组装（mock storage 返回内存流）=====

type memReader struct{ io.Reader }

func TestZipStream_MultiFile(t *testing.T) {
	svc, st, _, _ := newTestService(t)
	ctx := context.Background()

	resp, err := svc.CreateMultiFileShare(ctx, &MultiShareReq{
		Entries:      multiEntries(2, 10),
		OwnerIP:      "1.2.3.4",
		ExpiredCount: -1,
	})
	require.NoError(t, err)

	// mockStorage.GetFileReader 默认 (nil,0,nil)：为 zip 测试注入可读内容（每次新流）
	st.readerBytes = []byte("hello-file-content")

	fc, err := svc.GetFileByCode(ctx, resp.Code)
	require.NoError(t, err)

	rc, err := svc.ZipStream(ctx, fc)
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()

	buf, err := io.ReadAll(rc)
	require.NoError(t, err)

	zr, err := zip.NewReader(bytes.NewReader(buf), int64(len(buf)))
	require.NoError(t, err)
	require.Len(t, zr.File, 2)
	content, err := readZipEntry(zr.File[0])
	require.NoError(t, err)
	assert.Equal(t, "hello-file-content", content)
}

func readZipEntry(f *zip.File) (string, error) {
	rc, err := f.Open()
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	var b strings.Builder
	if _, err := io.Copy(&b, rc); err != nil {
		return "", err
	}
	return b.String(), nil
}

// ===== 子表是文件清单的唯一真相源（2026-10-26 虚拟行退役）=====

// TestListShareFiles_NoChildRow_NoVirtualItem 回归：主表有文件路径但没有子表行
// （存量未迁移形态）时，**不再合成「虚拟行（id=0）」**——存量数据由启动期数据
// 迁移补齐为真实子行（见 bootstrap/datamigrate.go）。
func TestListShareFiles_NoChildRow_NoVirtualItem(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()

	fc := &model.FileCode{
		Code:         "nolegacy01",
		FilePath:     "uploads/2025/01/01/old-uuid.pdf",
		UUIDFileName: "old-uuid.pdf",
		Text:         "报告.pdf",
		Size:         1234,
		ExpiredCount: -1,
	}
	require.NoError(t, svc.fileCodeRepo.Create(ctx, fc))

	items, err := svc.ListShareFiles(ctx, "nolegacy01")
	require.NoError(t, err)
	assert.Empty(t, items, "虚拟行已退役：无子表行即无文件项")
}

// TestListShareFiles_ChildRowIsSource 回归：文件清单来自子表（文件名取原始名）。
func TestListShareFiles_ChildRowIsSource(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()

	fc := &model.FileCode{
		Code:         "withchild01",
		FilePath:     "uploads/2025/01/01/uuid-name.pdf",
		UUIDFileName: "uuid-name.pdf",
		Text:         "报告.pdf",
		Size:         1234,
		ExpiredCount: -1,
	}
	require.NoError(t, svc.fileCodeRepo.Create(ctx, fc))
	require.NoError(t, svc.fileFileRepo.CreateBatch(ctx, []*model.FileCodeFile{{
		FileCodeID:   fc.ID,
		FileName:     "报告.pdf",
		UUIDFileName: "uuid-name.pdf",
		FilePath:     "uploads/2025/01/01/uuid-name.pdf",
		Size:         1234,
	}}))

	items, err := svc.ListShareFiles(ctx, "withchild01")
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.NotZero(t, items[0].ID, "子表行才有真实 id（虚拟行 id=0 已退役）")
	assert.Equal(t, "报告.pdf", items[0].Name, "展示名取原始文件名")
	assert.Equal(t, int64(1234), items[0].Size)
}
