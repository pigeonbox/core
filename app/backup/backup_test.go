package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/repo/db/model"
)

// ── 测试替身 ────────────────────────────────────────────────────────────────

// fakeLister 返回固定的对象清单。
type fakeLister struct{ paths []string }

func (f fakeLister) ListReferencedObjectPaths(ctx context.Context) ([]string, error) {
	return f.paths, nil
}

// fakeStore 内存对象存储（路径 → 内容）。
type fakeStore struct {
	data map[string][]byte
}

func newFakeStore() *fakeStore { return &fakeStore{data: map[string][]byte{}} }

func (f *fakeStore) GetFileReader(ctx context.Context, p string) (io.ReadCloser, int64, error) {
	b, ok := f.data[p]
	if !ok {
		return nil, 0, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

func (f *fakeStore) EffectiveType() string { return "local" }

func (f *fakeStore) FileExists(ctx context.Context, p string) bool {
	_, ok := f.data[p]
	return ok
}

func (f *fakeStore) SaveStream(ctx context.Context, p string, r io.Reader, size int64) (int64, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	f.data[p] = b
	return int64(len(b)), nil
}

// newTestDB 建一个**文件型** SQLite（VACUUM INTO 需要真实文件）。
func newTestDB(t *testing.T, dir string) *gorm.DB {
	t.Helper()
	path := filepath.Join(dir, "app.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.FileCode{}))
	return db
}

// seedFileCodes 写入分享记录。
func seedFileCodes(t *testing.T, db *gorm.DB, rows ...model.FileCode) {
	t.Helper()
	for i := range rows {
		require.NoError(t, db.Create(&rows[i]).Error)
	}
}

// ── 导出 ────────────────────────────────────────────────────────────────────

// TestExport_PackageShape 备份包结构：manifest/db/files/objects 齐全，摘要与计数正确。
func TestExport_PackageShape(t *testing.T) {
	dir := t.TempDir()
	db := newTestDB(t, dir)
	seedFileCodes(t, db,
		model.FileCode{Code: "ABCD12", FilePath: "uploads/2026/10/31/a.txt", Size: 5, Status: "normal"},
		model.FileCode{Code: "EFGH34", FilePath: "uploads/2026/10/31/b.txt", Size: 7, Status: "normal"},
	)

	store := newFakeStore()
	store.data["uploads/2026/10/31/a.txt"] = []byte("hello")
	store.data["uploads/2026/10/31/b.txt"] = []byte("worldly")

	svc := NewService(db, fakeLister{paths: []string{
		"uploads/2026/10/31/a.txt", "uploads/2026/10/31/b.txt",
	}}, store)

	var buf bytes.Buffer
	manifest, err := svc.Export(context.Background(), &buf)
	require.NoError(t, err)
	require.NotNil(t, manifest)
	assert.Equal(t, FormatVersion, manifest.BackupFormat)
	assert.Equal(t, int64(2), manifest.ObjectCount)
	assert.Equal(t, int64(12), manifest.ObjectBytes) // 5 + 7
	assert.Equal(t, int64(2), manifest.FileCount)
	require.Len(t, manifest.Objects, 2)
	for _, o := range manifest.Objects {
		assert.Len(t, o.SHA256, 64, "对象必须带 sha256")
	}

	// 包内条目
	names := map[string][]byte{}
	gz, err := gzip.NewReader(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		b, err := io.ReadAll(tr)
		require.NoError(t, err)
		names[hdr.Name] = b
	}
	require.Contains(t, names, ManifestName)
	require.Contains(t, names, DBEntryName)
	require.Contains(t, names, FileListName)
	require.Contains(t, names, ObjectDirName+"uploads/2026/10/31/a.txt")
	assert.Equal(t, "hello", string(names[ObjectDirName+"uploads/2026/10/31/a.txt"]))

	// manifest 内容与 files.json 内容
	var m Manifest
	require.NoError(t, json.Unmarshal(names[ManifestName], &m))
	assert.Equal(t, manifest.ObjectCount, m.ObjectCount)
	var fl FileList
	require.NoError(t, json.Unmarshal(names[FileListName], &fl))
	assert.Equal(t, 2, fl.Count)
	assert.Equal(t, "ABCD12", fl.Files[0].Code)
}

// TestExport_MissingObjectTolerated 存量缺失（DB 有引用、磁盘已无）⇒ 导出**不整体失败**，
// 且缺失对象被**如实记账**。
//
// 真机实录（2026-10-31，10.10.10.3）：这台实例的 DB 有 300+ 条分享记录，其中若干对象
// 文件早被清理 ⇒ 原实现「读不到就整体失败」⇒ **备份包在这台实例上根本导不出来**
// ⇒ 灾备能力在最需要它的机器上失效。
func TestExport_MissingObjectTolerated(t *testing.T) {
	dir := t.TempDir()
	db := newTestDB(t, dir)
	seedFileCodes(t, db, model.FileCode{Code: "GONE01", FilePath: "uploads/gone.txt", Size: 3, Status: "normal"})

	store := newFakeStore()
	// 清单里有 uploads/gone.txt，但 store 里没有它 ⇒ 存量缺失
	svc := NewService(db, fakeLister{paths: []string{"uploads/gone.txt"}}, store)

	var buf bytes.Buffer
	manifest, err := svc.Export(context.Background(), &buf)
	require.NoError(t, err, "存量缺失不得让整个导出失败")
	assert.Equal(t, int64(0), manifest.ObjectCount)
	require.Len(t, manifest.MissingObjects, 1)
	assert.Equal(t, "uploads/gone.txt", manifest.MissingObjects[0])

	// 包本身仍可用（manifest 在、DB 快照在）
	entries := readEntries(t, buf.Bytes())
	require.Contains(t, entries, ManifestName)
	require.Contains(t, entries, DBEntryName)
}

// TestExport_NonSQLite 非 SQLite 库明确报错（不产半成品包）。
//
// 换库（SQLite → PG/MySQL）不是本格式支持的：那种包恢复起来语义含糊
// ⇒ 这里宁可明确报错，让用户走 pg_dump/mysqldump 的专门流程。
func TestExport_NonSQLite(t *testing.T) {
	db := newTestDB(t, t.TempDir())
	svc := NewService(db, fakeLister{}, newFakeStore())
	svc.dbType = "mysql"
	_, err := svc.Export(context.Background(), &bytes.Buffer{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnsupportedDB)
}

// ── 恢复 ────────────────────────────────────────────────────────────────────

// TestRestore_RoundTrip 导出→恢复：对象内容一致、DB 替换成功、链接清单一致。
func TestRestore_RoundTrip(t *testing.T) {
	srcDir := t.TempDir()
	db := newTestDB(t, srcDir)
	seedFileCodes(t, db, model.FileCode{Code: "KEEP01", FilePath: "uploads/x.txt", Size: 3, Status: "normal"})

	store := newFakeStore()
	store.data["uploads/x.txt"] = []byte("abc")
	svc := NewService(db, fakeLister{paths: []string{"uploads/x.txt"}}, store)

	var buf bytes.Buffer
	_, err := svc.Export(context.Background(), &buf)
	require.NoError(t, err)

	// 目标实例：空对象存储 + 另一个 DB 文件
	dstDir := t.TempDir()
	dstDBPath := filepath.Join(dstDir, "app.db")
	dstDB := newTestDB(t, dstDir)
	_ = dstDB

	dstStore := newFakeStore()
	dstSvc := NewService(dstDB, fakeLister{}, dstStore)

	res, err := dstSvc.Restore(context.Background(), bytes.NewReader(buf.Bytes()), RestoreOptions{
		Confirm: true,
		DBPath:  dstDBPath,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), res.ObjectsRestored)
	assert.Equal(t, int64(3), res.BytesRestored)
	assert.True(t, res.DBRestored)
	assert.Equal(t, "abc", string(dstStore.data["uploads/x.txt"]))
	assert.Empty(t, res.MissingObjects)

	// DB 已被替换：打开目标文件应能看到快照里的分享记录
	restored, err := gorm.Open(sqlite.Open(dstDBPath), &gorm.Config{})
	require.NoError(t, err)
	var rows []model.FileCode
	require.NoError(t, restored.Find(&rows).Error)
	require.Len(t, rows, 1)
	assert.Equal(t, "KEEP01", rows[0].Code, "恢复后取件码必须不变")
}

// TestRestore_RequiresConfirm 未显式确认 ⇒ 拒绝（破坏性操作）。
func TestRestore_RequiresConfirm(t *testing.T) {
	dir := t.TempDir()
	db := newTestDB(t, dir)
	svc := NewService(db, fakeLister{}, newFakeStore())
	var buf bytes.Buffer
	_, err := svc.Export(context.Background(), &buf)
	require.NoError(t, err)

	_, err = svc.Restore(context.Background(), bytes.NewReader(buf.Bytes()), RestoreOptions{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRestoreNeedsConfirm)
}

// TestRestore_RejectsTamperedObject 对象被篡改 ⇒ 拒绝，且**一个字节都不落盘**。
func TestRestore_RejectsTamperedObject(t *testing.T) {
	dir := t.TempDir()
	db := newTestDB(t, dir)
	store := newFakeStore()
	store.data["uploads/x.txt"] = []byte("good")
	svc := NewService(db, fakeLister{paths: []string{"uploads/x.txt"}}, store)

	var buf bytes.Buffer
	_, err := svc.Export(context.Background(), &buf)
	require.NoError(t, err)

	// 篡改包内对象内容（重打包）
	tampered := tamperObject(t, buf.Bytes(), "uploads/x.txt", "evil")
	dst := newFakeStore()
	dstSvc := NewService(db, fakeLister{}, dst)

	_, err = dstSvc.Restore(context.Background(), bytes.NewReader(tampered), RestoreOptions{
		Confirm: true, DBPath: filepath.Join(t.TempDir(), "app.db"),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "摘要不符")
	assert.Empty(t, dst.data, "校验失败时不得写任何对象")
}

// TestRestore_RejectsPathTraversal 包内含 `../` 条目 ⇒ 拒绝（不能往机器上写任意文件）。
func TestRestore_RejectsPathTraversal(t *testing.T) {
	evil := buildTarGz(t, map[string][]byte{
		ManifestName:    []byte(`{"backup_format":1}`),
		"../escape.txt": []byte("pwned"),
	})
	svc := NewService(newTestDB(t, t.TempDir()), fakeLister{}, newFakeStore())
	_, err := svc.Restore(context.Background(), bytes.NewReader(evil), RestoreOptions{Confirm: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "非法条目名")
}

// TestRestore_RejectsUnknownFormat 不认识的格式版本 ⇒ 拒绝。
func TestRestore_RejectsUnknownFormat(t *testing.T) {
	dir := t.TempDir()
	db := newTestDB(t, dir)
	store := newFakeStore()
	store.data["uploads/x.txt"] = []byte("abc")
	svc := NewService(db, fakeLister{paths: []string{"uploads/x.txt"}}, store)
	var buf bytes.Buffer
	_, err := svc.Export(context.Background(), &buf)
	require.NoError(t, err)

	// 把 manifest 的版本号改成未来版本
	bad := replaceManifest(t, buf.Bytes(), func(m *Manifest) { m.BackupFormat = FormatVersion + 99 })
	_, err = svc.Restore(context.Background(), bytes.NewReader(bad), RestoreOptions{Confirm: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), UnsupportedFmt)
}

// TestRestore_DryRun 只校验不落盘。
func TestRestore_DryRun(t *testing.T) {
	dir := t.TempDir()
	db := newTestDB(t, dir)
	store := newFakeStore()
	store.data["uploads/x.txt"] = []byte("abc")
	svc := NewService(db, fakeLister{paths: []string{"uploads/x.txt"}}, store)
	var buf bytes.Buffer
	_, err := svc.Export(context.Background(), &buf)
	require.NoError(t, err)

	dst := newFakeStore()
	dstSvc := NewService(db, fakeLister{}, dst)
	res, err := dstSvc.Restore(context.Background(), bytes.NewReader(buf.Bytes()), RestoreOptions{DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, int64(0), res.ObjectsRestored)
	assert.Empty(t, dst.data, "dry-run 不得写对象")
}

// ── 工具 ────────────────────────────────────────────────────────────────────

// tamperObject 重打包并替换指定对象的内容。
func tamperObject(t *testing.T, pkg []byte, path, content string) []byte {
	t.Helper()
	entries := readEntries(t, pkg)
	entries[ObjectDirName+path] = []byte(content)
	return buildTarGz(t, entries)
}

// replaceManifest 重打包并替换 manifest 内容。
func replaceManifest(t *testing.T, pkg []byte, fn func(*Manifest)) []byte {
	t.Helper()
	entries := readEntries(t, pkg)
	var m Manifest
	require.NoError(t, json.Unmarshal(entries[ManifestName], &m))
	fn(&m)
	b, err := json.Marshal(m)
	require.NoError(t, err)
	entries[ManifestName] = b
	return buildTarGz(t, entries)
}

// readEntries 读出包内所有条目。
func readEntries(t *testing.T, pkg []byte) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	gz, err := gzip.NewReader(bytes.NewReader(pkg))
	require.NoError(t, err)
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		b, err := io.ReadAll(tr)
		require.NoError(t, err)
		out[hdr.Name] = b
	}
	return out
}

// buildTarGz 把条目打成 tar.gz。
func buildTarGz(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}))
		_, err := tw.Write(body)
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}
