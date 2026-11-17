// hub#364 强制测试②（L4/L7 可恢复备份）：恢复到**隔离临时目录**并**逐文件
// sha256 校验**——不是只断言产物存在。
//
// 与既有 TestRestore_RoundTrip 的区别：那条用内存 fakeStore 断言单文件内容；
// 本用例源与目标都是**真实 local 存储**（各自独立 DataPath），恢复后 walk
// 隔离目录，对 manifest 里每个对象：
//  1. 落盘文件的 sha256 == manifest.Objects[].SHA256（包内摘要）；
//  2. 落盘文件的 sha256 == 源实例原始内容的 sha256（逐字节一致）；
//  3. 隔离目录下的文件数 == manifest 对象数（不缺不多）。
//
// 正判据：三个对象全部命中、DB 已替换、MissingObjects 为空。
// 负对照：把 restore 落盘内容改坏一个字节 ⇒ sha256 断言转红（PR 附实跑结果）。
package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/pigeonbox/core/storage"
)

// diskStore 把真实 local 存储服务适配成备份域窄接口（bootstrap 装配同款）。
type diskStore struct{ s *storage.StorageService }

func (d diskStore) GetFileReader(ctx context.Context, p string) (io.ReadCloser, int64, error) {
	return d.s.GetFileReader(ctx, p)
}
func (d diskStore) EffectiveType() string { return string(d.s.EffectiveType()) }
func (d diskStore) SaveStream(ctx context.Context, p string, r io.Reader, size int64) (int64, error) {
	return d.s.SaveStream(ctx, p, r, size)
}
func (d diskStore) FileExists(ctx context.Context, p string) bool { return d.s.FileExists(ctx, p) }

// ObjectExists 错误可见的存在性探测（转发真实存储层：缺失 ⇒ (false, nil)，
// 后端故障 ⇒ 如实报错）。
func (d diskStore) ObjectExists(ctx context.Context, p string) (bool, error) {
	return d.s.ObjectExists(ctx, p)
}

// TestRestore_IsolatedDir_PerFileSHA256 恢复到隔离临时目录 + 逐文件 sha256。
func TestRestore_IsolatedDir_PerFileSHA256(t *testing.T) {
	ctx := context.Background()

	// 源实例：文件 DB + 真实 local 存储，三个对象（ASCII / 二进制 / CJK 路径）。
	contents := map[string][]byte{
		"uploads/a.txt":      []byte("pigeonbox backup restore probe\n"),
		"uploads/sub/b.bin":  {0x00, 0x01, 0x02, 0xff, 0xfe, 0x7f, 0x80},
		"uploads/中文目录/c.txt": []byte("中文内容：可恢复备份逐文件校验"),
	}
	srcStore := storage.NewStorageService(&storage.StorageConfig{
		Type:     storage.StorageTypeLocal,
		DataPath: t.TempDir(),
	})
	var paths []string
	for p, b := range contents {
		_, err := srcStore.SaveStream(ctx, p, bytes.NewReader(b), int64(len(b)))
		require.NoError(t, err, "源对象写入 %q", p)
		paths = append(paths, p)
	}

	srcDBDir := t.TempDir()
	srcDB := newTestDB(t, srcDBDir)
	for _, p := range paths {
		seedFileCodes(t, srcDB, model.FileCode{
			Code: "ISO" + filepath.Base(p), FilePath: p, Size: int64(len(contents[p])), Status: "normal",
		})
	}
	svc := NewService(srcDB, fakeLister{paths: paths}, diskStore{srcStore})
	var buf bytes.Buffer
	_, err := svc.Export(ctx, &buf)
	require.NoError(t, err)

	// 目标实例：**隔离**存储根 + 独立 DB 文件（与源零共享）。
	dstData := t.TempDir()
	dstStore := storage.NewStorageService(&storage.StorageConfig{
		Type:     storage.StorageTypeLocal,
		DataPath: dstData,
	})
	dstDBDir := t.TempDir()
	dstDB := newTestDB(t, dstDBDir)
	dstDBPath := filepath.Join(dstDBDir, "app.db")
	dstSvc := NewService(dstDB, fakeLister{}, diskStore{dstStore})

	res, err := dstSvc.Restore(ctx, bytes.NewReader(buf.Bytes()), RestoreOptions{
		Confirm: true,
		DBPath:  dstDBPath,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(len(paths)), res.ObjectsRestored)
	assert.True(t, res.DBRestored)
	assert.Empty(t, res.MissingObjects)

	// 逐文件 sha256：落盘 == manifest 摘要 == 源原始内容。
	require.NotNil(t, res.Manifest)
	byPath := map[string]string{}
	for _, e := range res.Manifest.Objects {
		byPath[e.Path] = e.SHA256
	}
	require.Len(t, res.Manifest.Objects, len(paths))

	onDisk := 0
	err = filepath.WalkDir(dstData, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if !d.IsDir() {
			onDisk++
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, len(paths), onDisk, "隔离目录里的文件数必须恰等于 manifest 对象数（不缺不多）")

	for _, p := range paths {
		wantManifest, ok := byPath[p]
		require.True(t, ok, "manifest 缺对象 %q", p)
		got, rerr := os.ReadFile(filepath.Join(dstData, filepath.FromSlash(p)))
		require.NoError(t, rerr, "隔离目录缺恢复出的文件 %q", p)
		sum := sha256.Sum256(got)
		gotHex := hex.EncodeToString(sum[:])
		assert.Equal(t, wantManifest, gotHex, "对象 %q 落盘 sha256 必须等于 manifest 摘要", p)

		srcSum := sha256.Sum256(contents[p])
		assert.Equal(t, hex.EncodeToString(srcSum[:]), gotHex,
			"对象 %q 落盘 sha256 必须等于源实例原始内容（逐字节一致）", p)
	}

	// DB 已被替换：恢复出的分享记录与源一致。
	restored, rerr := gorm.Open(sqlite.Open(dstDBPath), &gorm.Config{})
	require.NoError(t, rerr)
	var rows []model.FileCode
	require.NoError(t, restored.Find(&rows).Error)
	assert.Len(t, rows, len(paths), "恢复后分享记录数必须与源一致")
}
