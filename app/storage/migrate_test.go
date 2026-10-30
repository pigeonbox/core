package storage

// 存储迁移的行为断言（2026-10-30）。
//
// 守卫点（四个，缺一即失效）：
//  1. **搬完才切换**：全部对象复制成功后 Run 才 Reload（切换）后端；
//  2. **失败不切换**：任一对象失败 ⇒ 不切换，源后端继续在用（线上零影响）；
//  3. **续跑跳过**：目标已存在的对象跳过（可中断可续）；
//  4. **同源拒绝**：目标=源 ⇒ 明确报错，不做无意义的自我复制。

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pigeonbox/core/conf"
	corestorage "github.com/pigeonbox/core/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLister 假的对象清单来源（测试只关心路径集合）。
type fakeLister struct {
	paths []string
	err   error
}

func (f *fakeLister) ListReferencedObjectPaths(context.Context) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.paths, nil
}

// newMigrateTestService 装一个「真实存储单例 + 真实对象」的迁移环境。
//
// 用真实的 core/storage 单例当 runtime（它实现 ObjectReader）——
// 迁移必须真读写对象，用假 runtime 测不出「对象没搬过去」这类缺陷。
func newMigrateTestService(t *testing.T, srcDir string, paths []string) (*Service, *corestorage.StorageService, *fakePersister) {
	t.Helper()
	conf.SetGlobalConfig(&conf.AppConfiguration{
		Server:  conf.ServerConfig{Host: "127.0.0.0", Port: 12345},
		Storage: conf.StorageConfig{Type: "local", StoragePath: srcDir},
	})
	rt, err := corestorage.NewStorageServiceE(corestorage.ConfigFromConf(
		&conf.StorageConfig{Type: "local", StoragePath: srcDir}, ""))
	require.NoError(t, err)
	p := &fakePersister{}
	s := NewService()
	s.SetRuntime(rt)
	s.SetPersister(p)
	s.SetObjectLister(&fakeLister{paths: paths})
	return s, rt, p
}

func TestMigrateStorage_LocalToLocal_MovesObjectsAndSwitches(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "target")

	s, rt, persister := newMigrateTestService(t, src, []string{"a/1.txt", "b/2.txt"})
	require.NoError(t, rt.SaveBytes(ctx, "a/1.txt", []byte("hello-a")))
	require.NoError(t, rt.SaveBytes(ctx, "b/2.txt", []byte("hello-b")))

	res, err := s.MigrateStorage(ctx, &MigrateStorageReq{TargetType: "local", TargetPath: dst})
	require.NoError(t, err, "迁移应成功：%v", err)
	assert.True(t, res.Switched, "全部复制成功后必须切换")
	assert.Equal(t, int64(2), res.Copied)
	assert.Equal(t, int64(0), res.Failed)
	assert.Equal(t, int64(0), res.Skipped)
	assert.Equal(t, int64(2), res.Objects)

	// 对象真的落到目标目录（且内容一致）
	data, err := os.ReadFile(filepath.Join(dst, "a/1.txt"))
	require.NoError(t, err, "对象应出现在目标目录")
	assert.Equal(t, "hello-a", string(data))
	data, err = os.ReadFile(filepath.Join(dst, "b/2.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello-b", string(data))

	// 切换已持久化（重启不丢）
	require.Len(t, persister.saved, 1, "切换必须写穿持久化")
	assert.Equal(t, dst, persister.saved[0].StoragePath)
}

func TestMigrateStorage_SourceObjectsUntouched(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "target")

	s, rt, _ := newMigrateTestService(t, src, []string{"a/1.txt"})
	require.NoError(t, rt.SaveBytes(ctx, "a/1.txt", []byte("keep")))

	_, err := s.MigrateStorage(ctx, &MigrateStorageReq{TargetType: "local", TargetPath: dst})
	require.NoError(t, err)

	// 源目录的对象**仍在**（迁移是复制不是移动）⇒ 失败时可回退，源端不受损
	_, err = os.Stat(filepath.Join(src, "a/1.txt"))
	assert.NoError(t, err, "源对象不应被删除")
}

func TestMigrateStorage_FailureDoesNotSwitch(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "target")

	// 清单里有一个**不存在**的对象 ⇒ 复制失败
	s, rt, persister := newMigrateTestService(t, src, []string{"a/ok.txt", "a/missing.txt"})
	require.NoError(t, rt.SaveBytes(ctx, "a/ok.txt", []byte("ok")))

	res, err := s.MigrateStorage(ctx, &MigrateStorageReq{TargetType: "local", TargetPath: dst})
	require.Error(t, err, "有对象失败时必须报错")
	require.NotNil(t, res)
	assert.False(t, res.Switched, "**失败时不得切换**（源后端继续在用）")
	assert.Equal(t, int64(1), res.Failed)
	assert.Equal(t, int64(1), res.Copied, "成功的那个仍应复制过去")
	assert.Empty(t, persister.saved, "失败时不得持久化切换")
}

func TestMigrateStorage_ResumeSkipsExisting(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "target")

	s, rt, _ := newMigrateTestService(t, src, []string{"a/1.txt"})
	require.NoError(t, rt.SaveBytes(ctx, "a/1.txt", []byte("v1")))

	// 先在目标目录放一个同名对象（模拟上次搬了一半）
	require.NoError(t, os.MkdirAll(filepath.Join(dst, "a"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dst, "a/1.txt"), []byte("v1"), 0o644))

	res, err := s.MigrateStorage(ctx, &MigrateStorageReq{TargetType: "local", TargetPath: dst})
	require.NoError(t, err)
	assert.Equal(t, int64(1), res.Skipped, "目标已存在的对象应跳过（可中断可续）")
	assert.Equal(t, int64(0), res.Copied)
	assert.True(t, res.Switched)
}

func TestMigrateStorage_DryRunDoesNothing(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "target")

	s, rt, persister := newMigrateTestService(t, src, []string{"a/1.txt"})
	require.NoError(t, rt.SaveBytes(ctx, "a/1.txt", []byte("x")))

	res, err := s.MigrateStorage(ctx, &MigrateStorageReq{TargetType: "local", TargetPath: dst, DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, int64(1), res.Objects, "dry-run 也要报出对象数")
	assert.Equal(t, int64(0), res.Copied)
	assert.False(t, res.Switched, "dry-run 不得切换")
	assert.Empty(t, persister.saved)
	_, err = os.Stat(filepath.Join(dst, "a/1.txt"))
	assert.True(t, os.IsNotExist(err), "dry-run 不得复制任何对象")
}

func TestMigrateStorage_SameBackendRejected(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()

	s, _, _ := newMigrateTestService(t, src, []string{"a/1.txt"})
	_, err := s.MigrateStorage(ctx, &MigrateStorageReq{TargetType: "local"})
	require.Error(t, err, "目标=源必须明确拒绝")
	assert.Contains(t, err.Error(), "相同")
}

func TestMigrateStorage_UnsupportedTypeRejected(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()

	s, _, _ := newMigrateTestService(t, src, nil)
	_, err := s.MigrateStorage(ctx, &MigrateStorageReq{TargetType: "nope"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "不支持")
}
