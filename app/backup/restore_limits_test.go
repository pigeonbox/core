// core#221：备份恢复解包资源上限（条目数 / 解包总量 / 压缩比 + manifest 对账）。
//
// 修复前 unpack 只做路径维度防御（safeEntryName + Rel），资源维度完全无上限：
//   - for { tr.Next() } 无计数 ⇒ 百万条目包可刷爆临时目录；
//   - io.Copy 逐条无总量校验 ⇒ 声明 1MB 实际 10GB 可填盘；
//   - gzip 解压炸弹（高压缩比）直接喂给 tar reader。
//
// 本文件用**小阈值注入**验证三类上限都在写满磁盘前拒绝，并验证 Restore 的
// 失败路径会清理解包临时目录。
package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tarEntry 测试包内一个条目。
type limitTarEntry struct {
	name string
	body []byte
	typ  byte
}

// buildTarGz 构造 tar.gz（目录条目 body 为空）。
func buildLimitTarGz(t *testing.T, entries []limitTarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Mode: 0o600, Size: int64(len(e.body)), Typeflag: typ}
		require.NoError(t, tw.WriteHeader(hdr))
		if len(e.body) > 0 {
			_, err := tw.Write(e.body)
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

func manifestJSON(t *testing.T, m Manifest) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	require.NoError(t, err)
	return b
}

// TestUnpackRejectsTooManyEntries 条目数上限：目录/链接/普通文件全部计入。
func TestUnpackRejectsTooManyEntries(t *testing.T) {
	dir := t.TempDir()
	pack := buildLimitTarGz(t, []limitTarEntry{
		{name: "a", body: []byte("1")},
		{name: "b", body: []byte("2")},
		{name: "c", body: []byte("3")},
		{name: ManifestName, body: manifestJSON(t, Manifest{BackupFormat: FormatVersion})},
	})
	svc := &Service{}
	_, err := svc.unpack(bytes.NewReader(pack), dir, UnpackLimits{
		MaxEntries: 3, MaxTotalBytes: 1 << 20, MaxCompressionRatio: 1000,
	})
	require.Error(t, err, "超过条目数上限必须拒绝")
	assert.Contains(t, err.Error(), "条目数")
}

// TestUnpackRejectsTotalBytes 解包总量上限：按实际写入字节（不信 hdr.Size）。
func TestUnpackRejectsTotalBytes(t *testing.T) {
	dir := t.TempDir()
	pack := buildLimitTarGz(t, []limitTarEntry{
		{name: "big.bin", body: bytes.Repeat([]byte("x"), 4096)},
		{name: ManifestName, body: manifestJSON(t, Manifest{BackupFormat: FormatVersion})},
	})
	svc := &Service{}
	_, err := svc.unpack(bytes.NewReader(pack), dir, UnpackLimits{
		MaxEntries: 100, MaxTotalBytes: 1024, MaxCompressionRatio: 1000,
	})
	require.Error(t, err, "超过解包总量上限必须拒绝")
	assert.Contains(t, err.Error(), "总量")
}

// TestUnpackRejectsCompressionBomb 压缩比上限：32KB 零字节压缩后仅几十字节。
func TestUnpackRejectsCompressionBomb(t *testing.T) {
	dir := t.TempDir()
	pack := buildLimitTarGz(t, []limitTarEntry{
		{name: "bomb.bin", body: make([]byte, 32<<10)},
		{name: ManifestName, body: manifestJSON(t, Manifest{BackupFormat: FormatVersion})},
	})
	svc := &Service{}
	_, err := svc.unpack(bytes.NewReader(pack), dir, UnpackLimits{
		MaxEntries: 100, MaxTotalBytes: 10 << 20, MaxCompressionRatio: 5,
		CompressionRatioMinBytes: 1024,
	})
	require.Error(t, err, "压缩比超限必须拒绝（gzip 炸弹防御）")
	assert.Contains(t, err.Error(), "压缩比")
}

// TestUnpackRejectsManifestObjectBytesMismatch manifest 申报对象总量 1KB、
// 实际对象 2MB ⇒ 解包后对账拒绝（issue 的「声明 1MB 实际 10GB」小尺度版）。
func TestUnpackRejectsManifestObjectBytesMismatch(t *testing.T) {
	dir := t.TempDir()
	pack := buildLimitTarGz(t, []limitTarEntry{
		{name: ObjectDirName + "big.bin", body: bytes.Repeat([]byte("z"), 2<<20)},
		{name: ManifestName, body: manifestJSON(t, Manifest{
			BackupFormat: FormatVersion,
			ObjectBytes:  1 << 10, // 声明 1KB
			Objects:      []ObjectEntry{{Path: "big.bin", Size: 1 << 10}},
		})},
	})
	svc := &Service{}
	_, err := svc.unpack(bytes.NewReader(pack), dir, UnpackLimits{
		MaxEntries: 100, MaxTotalBytes: 10 << 20, MaxCompressionRatio: 1000,
	})
	require.Error(t, err, "实际对象字节数超过 manifest 申报 ⇒ 必须拒绝")
	assert.Contains(t, err.Error(), "manifest")
}

// TestUnpackVerifyObjectsSizeMismatch verifyObjects 按 manifest 声明大小精确比对
// （补齐「sha256 只读 entry.Size 字节」留下的尾部追加盲区）。
func TestUnpackVerifyObjectsSizeMismatch(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.FromSlash(ObjectDirName)), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "objects", "a.bin"), []byte("123456"), 0o600))
	svc := &Service{}
	err := svc.verifyObjects(dir, &Manifest{
		Objects: []ObjectEntry{{Path: "a.bin", Size: 3, SHA256: "x"}},
	})
	require.Error(t, err, "磁盘大小与 manifest 声明不符必须拒绝")
	assert.Contains(t, err.Error(), "大小")
}

// TestRestoreUnpackLimitCleansTempDir 失败路径清理解包临时目录。
func TestRestoreUnpackLimitCleansTempDir(t *testing.T) {
	tmpRoot := t.TempDir()
	t.Setenv("TMPDIR", tmpRoot)

	pack := buildLimitTarGz(t, []limitTarEntry{
		{name: "big.bin", body: bytes.Repeat([]byte("x"), 8192)},
		{name: ManifestName, body: manifestJSON(t, Manifest{BackupFormat: FormatVersion})},
	})
	svc := &Service{store: newFakeStore()}
	_, err := svc.Restore(context.Background(), bytes.NewReader(pack), RestoreOptions{
		Confirm: true,
		Limits: UnpackLimits{
			MaxEntries: 100, MaxTotalBytes: 1024, MaxCompressionRatio: 1000,
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "总量")

	entries, rerr := os.ReadDir(tmpRoot)
	require.NoError(t, rerr)
	assert.Empty(t, entries, "解包失败后不得残留 pb-restore-* 临时目录")
}
