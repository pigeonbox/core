package storage

// object_exists_test.go —— ObjectExists 的契约（错误可见的存在性探测）。
//
// 断言（全部是行为）：
//   ① 真·对象不存在 ⇒ (false, nil)（**不是**错误）；
//   ② 后端不可达（Stat 失败）⇒ **如实报错**（不得吞成 (false, nil)）；
//   ③ 本地后端：存在 ⇒ (true, nil)；缺失 ⇒ (false, nil)；路径越界 ⇒ 报错。
//
// 为什么单列：FileExists 对**任何** Stat 错误都返回 false（吞错）⇒ 备份域
// 曾因此在后端宕机时把每个对象记成 missing、产出「看着成功、实际缺对象」的
// 灾备包（2026-11-18）。本用例钉住「错误可见」这条新契约。
//
// 负对照：把 ObjectExists 里的错误上抛改成吞错（返回 false, nil）⇒ ②红。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestObjectExists_LocalBackend 本地后端：存在 / 缺失 / 路径越界。
func TestObjectExists_LocalBackend(t *testing.T) {
	dir := t.TempDir()
	svc := NewStorageService(&StorageConfig{Type: StorageTypeLocal, DataPath: dir})
	ctx := context.Background()

	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o600))

	ok, err := svc.ObjectExists(ctx, "a.txt")
	require.NoError(t, err, "存在的对象不得报错")
	assert.True(t, ok, "存在的对象必须判为存在")

	ok, err = svc.ObjectExists(ctx, "missing.txt")
	require.NoError(t, err, "真·缺失不得报错（否则备份域无法区分存量缺失）")
	assert.False(t, ok, "真·缺失必须判为不存在")

	// 路径越界 ⇒ 报错（不得当成「不存在」蒙混过去）。
	_, err = svc.ObjectExists(ctx, "../escape.txt")
	require.Error(t, err, "越界路径必须报错（不得当成对象不存在）")
}

// TestObjectExists_RemoteBackendDown_PropagatesError 远端后端不可达 ⇒ 如实报错。
//
// 复用 storage/fault_driver_test.go 的 faultDriver（Stat 可注入失败）。
func TestObjectExists_RemoteBackendDown_PropagatesError(t *testing.T) {
	fd := newFaultDriver()
	svc := newFaultStorageService(fd)
	ctx := context.Background()

	// 真·缺失 ⇒ (false, nil)。
	ok, err := svc.ObjectExists(ctx, "uploads/missing.bin")
	require.NoError(t, err, "真·缺失不得报错")
	assert.False(t, ok, "真·缺失必须判为不存在")

	// 后端不可达 ⇒ 报错（不得吞成 (false, nil)）。
	fd.setStatErr(errors.New("backend unavailable: 503 Service Unavailable"))
	ok, err = svc.ObjectExists(ctx, "uploads/x.bin")
	require.Error(t, err, "后端故障必须如实上报（吞成「不存在」⇒ 灾备包静默漏对象）")
	assert.False(t, ok)
	assert.NotErrorIs(t, err, os.ErrNotExist, "后端故障不得伪装成「对象不存在」")
}
