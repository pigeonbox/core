package share

// reldir_test.go —— 目录上传（5.3.0）在 share 域的守卫：
// 相对目录贯穿落库/取件清单，zip 保留结构，且路径不可能逃出解压根（Zip Slip）。

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ===== 相对目录贯穿落库与取件清单 =====

func TestCreateMultiFileShare_RelDirRoundTrip(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()

	resp, err := svc.CreateMultiFileShare(ctx, &MultiShareReq{
		Entries: []StoredFileEntry{
			{RelPath: "uploads/1.txt", FileName: "a.txt", RelDir: "photos/2024", Size: 10},
			{RelPath: "uploads/2.txt", FileName: "b.txt", RelDir: "photos", Size: 10},
			{RelPath: "uploads/3.txt", FileName: "c.txt", Size: 10},
		},
		OwnerIP:      "1.2.3.4",
		ExpiredCount: -1,
	})
	require.NoError(t, err)

	items, err := svc.ListShareFiles(ctx, resp.Code)
	require.NoError(t, err)
	require.Len(t, items, 3)
	// 顺序即 SortOrder（= entries 顺序）
	assert.Equal(t, "photos/2024", items[0].RelDir)
	assert.Equal(t, "photos", items[1].RelDir)
	assert.Equal(t, "", items[2].RelDir, "根目录下的文件 rel_dir 必须为空串（不是 \".\" 也不是 \"/\"）")
}

// 不同目录下的同名文件必须共存——这是目录上传的核心场景，按文件名判重会把
// 正常上传打成 400。
func TestCreateMultiFileShare_SameNameDifferentDir(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()

	resp, err := svc.CreateMultiFileShare(ctx, &MultiShareReq{
		Entries: []StoredFileEntry{
			{RelPath: "uploads/1.txt", FileName: "readme.txt", RelDir: "a", Size: 1},
			{RelPath: "uploads/2.txt", FileName: "readme.txt", RelDir: "b", Size: 1},
			{RelPath: "uploads/3.txt", FileName: "readme.txt", Size: 1},
		},
		OwnerIP:      "1.2.3.4",
		ExpiredCount: -1,
	})
	require.NoError(t, err)

	items, err := svc.ListShareFiles(ctx, resp.Code)
	require.NoError(t, err)
	require.Len(t, items, 3)
	assert.Equal(t, "a/readme.txt", fullNameOf(items, 0))
	assert.Equal(t, "b/readme.txt", fullNameOf(items, 1))
	assert.Equal(t, "readme.txt", fullNameOf(items, 2))
}

// 同目录同名：整单拒绝（不能静默改名——改名后清单与用户选的文件夹对不上）。
func TestCreateMultiFileShare_DuplicatePathRejected(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()

	_, err := svc.CreateMultiFileShare(ctx, &MultiShareReq{
		Entries: []StoredFileEntry{
			{RelPath: "uploads/1.txt", FileName: "same.txt", RelDir: "a", Size: 1},
			{RelPath: "uploads/2.txt", FileName: "same.txt", RelDir: "a", Size: 1},
		},
		OwnerIP:      "1.2.3.4",
		ExpiredCount: -1,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "重复文件")
	assert.Contains(t, err.Error(), "a/same.txt", "错误里必须给出冲突的完整路径，否则用户无从定位")
}

// fullNameOf 取件清单项的用户视角完整路径（测试内手拼：ShareFileItem 本身不带
// 拼接方法，避免把 Z 层口径塞进 A 层结构体）
func fullNameOf(items []*ShareFileItem, i int) string {
	if items[i].RelDir == "" {
		return items[i].Name
	}
	return items[i].RelDir + "/" + items[i].Name
}

// ===== zip 保留目录结构 =====

func TestZipStream_PreservesDirStructure(t *testing.T) {
	svc, st, _, _ := newTestService(t)
	ctx := context.Background()

	resp, err := svc.CreateMultiFileShare(ctx, &MultiShareReq{
		Entries: []StoredFileEntry{
			{RelPath: "uploads/1.txt", FileName: "readme.txt", RelDir: "docs/guide", Size: 5},
			{RelPath: "uploads/2.txt", FileName: "readme.txt", RelDir: "docs", Size: 5},
			{RelPath: "uploads/3.txt", FileName: "index.txt", Size: 5},
		},
		OwnerIP:      "1.2.3.4",
		ExpiredCount: -1,
	})
	require.NoError(t, err)
	st.readerBytes = []byte("hello")

	fc, err := svc.GetFileByCode(ctx, resp.Code)
	require.NoError(t, err)

	rc, err := svc.ZipStream(ctx, fc)
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()

	buf, err := io.ReadAll(rc)
	require.NoError(t, err)
	zr, err := zip.NewReader(bytes.NewReader(buf), int64(len(buf)))
	require.NoError(t, err)

	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	// 三级目录原样保留；两个 readme.txt 各自留在自己目录里（不因重名被改名/挪到根下）
	assert.Equal(t, []string{"docs/guide/readme.txt", "docs/readme.txt", "index.txt"}, names)
}

// ===== Zip Slip：路径逃逸的第二道闸（第一道是入库前的 SanitizeRelStrict）=====

func TestZipPath_BlocksSlip(t *testing.T) {
	cases := []struct {
		name     string
		fullName string
		want     string
	}{
		{"上跳段被丢弃", "../../evil.txt", "evil.txt"},
		{"目录段里的上跳段也被丢弃", "a/../../b/x.txt", "a/b/x.txt"},
		{"绝对路径被拍平为相对", "/etc/passwd", "etc/passwd"},
		{"Windows 分隔符归一", "a\\b\\c.txt", "a/b/c.txt"},
		{"当前目录段丢弃", "./a/./b.txt", "a/b.txt"},
		{"空段丢弃", "a//b.txt", "a/b.txt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, zipPath(c.fullName, nil))
		})
	}
}

// 去重按完整路径：不同目录的同名文件互不干扰；同路径才追加序号。
func TestZipPath_DedupesByFullPath(t *testing.T) {
	used := []string{"a/readme.txt"}
	assert.Equal(t, "b/readme.txt", zipPath("b/readme.txt", used), "不同目录不参与去重")
	assert.Equal(t, "a/readme(1).txt", zipPath("a/readme.txt", used), "同路径追加序号")
}

// FullName 是取件页与 zip 的共同口径（根下文件不带前导 "/"）。
func TestFileCodeFileFullName(t *testing.T) {
	assert.Equal(t, "x.txt", (&model.FileCodeFile{FileName: "x.txt"}).FullName())
	assert.Equal(t, "a/b/x.txt", (&model.FileCodeFile{FileName: "x.txt", RelDir: "a/b"}).FullName())
	assert.Equal(t, "a/x.txt", (&model.FileCodeFile{FileName: "x.txt", RelDir: "/a/"}).FullName(),
		"前后斜杠不能出现在展示路径里")
	// 空 FileName 回退 UUID 名/路径末段（既有语义不变）
	assert.Equal(t, "a/0f3d.bin", (&model.FileCodeFile{RelDir: "a", UUIDFileName: "0f3d.bin"}).FullName())
}
