package share

// 目录配额（core#159）执行面断言。
//
// 守卫点：
//  1. 超限 ⇒ **落盘前**拒绝（不落文件、不建行）⇒ typed error（30002）；
//  2. 用量按 file_codes.dir_root 归因、只算**存活**行；
//  3. 并发导入**不超放**（在途预留参与临界区内的原子判定）；
//  4. 归因只记策略 root，不记用户原始绝对路径。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pigeonbox/contracts/errcode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/repo/db/model"
)

// dirQuotaPastTime 一个「已过期」的时间点（用于构造过期行）。
func dirQuotaPastTime() *time.Time {
	t := time.Now().Add(-time.Hour)
	return &t
}

// withDirectoryPolicy 装一份「本地导入已启用 + 目录策略」的全局配置。
func withDirectoryPolicy(t *testing.T, root string, quota int64, mode string) {
	t.Helper()
	old := conf.GetGlobalConfig()
	t.Cleanup(func() { conf.SetGlobalConfig(old) })
	conf.SetGlobalConfig(&conf.AppConfiguration{
		Upload: conf.UploadConfig{LocalImport: conf.LocalImportConfig{
			Enabled: true, Roots: []string{root},
		}},
		Share: conf.ShareConfig{DirectoryPolicies: []conf.DirectoryPolicyConfig{
			{Root: root, QuotaBytes: quota, ShareMode: mode},
		}},
	})
}

// writeLocalFile 在 dir 下写一个指定大小的文件，返回路径。
func writeLocalFile(t *testing.T, dir, name string, size int) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, bytes.Repeat([]byte("x"), size), 0o644))
	return p
}

// TestImportLocalFile_DirectoryQuota_RejectedBeforePersist 超限 ⇒ 落盘前拒绝。
func TestImportLocalFile_DirectoryQuota_RejectedBeforePersist(t *testing.T) {
	root := t.TempDir()
	p := writeLocalFile(t, root, "a.txt", 100)
	withDirectoryPolicy(t, root, 50, "allow")

	svc, gormDB, st, _, _ := newTestServiceWithDB(t)

	_, err := svc.ImportLocalFile(context.Background(), ImportLocalOpts{AbsPath: p})
	require.Error(t, err)

	// typed error ⇒ 业务码 30002（handler 透传 ⇒ HTTP 507）
	var coder interface{ ErrCode() int }
	require.True(t, errors.As(err, &coder), "必须返回 typed error: %v", err)
	assert.Equal(t, errcode.CodeStorageQuota, coder.ErrCode(), "目录配额超限的业务码必须是 30002")

	// 文案点名具体目录与限额（运维可直接定位）
	rootReal, rerr := filepath.EvalSymlinks(root)
	require.NoError(t, rerr)
	assert.Contains(t, err.Error(), rootReal, "错误文案必须点名具体目录")
	assert.Contains(t, err.Error(), "50", "错误文案必须点名限额")

	// 落盘前拒绝 ⇒ 存储零副作用 + 零建行
	assert.Equal(t, int64(0), st.saveStreamCalls.Load(), "超限必须在落盘前拒绝（不得写存储）")
	var rows int64
	require.NoError(t, gormDB.Model(&model.FileCode{}).Count(&rows).Error)
	assert.Equal(t, int64(0), rows, "超限必须在建行前拒绝（不得留行）")
}

// TestImportLocalFile_DirectoryQuota_UsageAccumulates 用量按目录累计、归因落 dir_root。
func TestImportLocalFile_DirectoryQuota_UsageAccumulates(t *testing.T) {
	root := t.TempDir()
	p1 := writeLocalFile(t, root, "a.txt", 100)
	p2 := writeLocalFile(t, root, "b.txt", 100)
	withDirectoryPolicy(t, root, 150, "allow")

	svc, gormDB, _, _, _ := newTestServiceWithDB(t)

	// 第一笔：100 ≤ 150 ⇒ 放行
	_, err := svc.ImportLocalFile(context.Background(), ImportLocalOpts{AbsPath: p1})
	require.NoError(t, err)

	// 归因：行上记的是**策略 root**（解析后形态），不是用户原始绝对路径
	rootReal, rerr := filepath.EvalSymlinks(root)
	require.NoError(t, rerr)
	var fc model.FileCode
	require.NoError(t, gormDB.Where("file_path <> ''").First(&fc).Error)
	assert.Equal(t, rootReal, fc.DirRoot, "归因只记命中的策略 root")
	assert.Equal(t, int64(100), fc.Size)

	// 第二笔：100(存活合计) + 100 > 150 ⇒ 拒绝
	_, err = svc.ImportLocalFile(context.Background(), ImportLocalOpts{AbsPath: p2})
	require.Error(t, err)
	var coder interface{ ErrCode() int }
	require.True(t, errors.As(err, &coder))
	assert.Equal(t, errcode.CodeStorageQuota, coder.ErrCode())

	// 只有第一笔建了行
	var rows int64
	require.NoError(t, gormDB.Model(&model.FileCode{}).Count(&rows).Error)
	assert.Equal(t, int64(1), rows)
}

// TestImportLocalFile_DirectoryQuota_ExpiredRowNotCounted 过期行不占目录额度。
//
// 口径：目录配额算**存活**分享（未软删、未过期）——与站点级物理占用口径
// 故意不同（站点量物理占用、含回收站软删行）。
func TestImportLocalFile_DirectoryQuota_ExpiredRowNotCounted(t *testing.T) {
	root := t.TempDir()
	withDirectoryPolicy(t, root, 100, "allow")

	svc, gormDB, _, _, _ := newTestServiceWithDB(t)
	rootReal, rerr := filepath.EvalSymlinks(root)
	require.NoError(t, rerr)

	// 一条**已过期**的归因行（100 字节）⇒ 不应计入目录用量
	require.NoError(t, gormDB.Create(&model.FileCode{
		Code: "EXP0001", FilePath: "uploads/x/exp.txt", Text: "exp.txt", Size: 100,
		DirRoot: rootReal, ExpiredCount: -1,
		ExpiredAt: dirQuotaPastTime(),
	}).Error)

	// 一条已软删的归因行（100 字节）⇒ 也不应计入
	require.NoError(t, gormDB.Create(&model.FileCode{
		Code: "DEL0001", FilePath: "uploads/x/del.txt", Text: "del.txt", Size: 100,
		DirRoot: rootReal, ExpiredCount: -1,
	}).Error)
	var del model.FileCode
	require.NoError(t, gormDB.Where("code = ?", "DEL0001").First(&del).Error)
	require.NoError(t, gormDB.Delete(&del).Error)

	// 额度 100 ⇒ 目录实际用量为 0 ⇒ 100 字节的导入仍应放行
	p := writeLocalFile(t, root, "ok.txt", 100)
	_, err := svc.ImportLocalFile(context.Background(), ImportLocalOpts{AbsPath: p})
	require.NoError(t, err, "过期/已删行不占目录额度")
}

// TestReserveDirectoryQuota_ConcurrentNoOvercommit 并发预留不得超放。
//
// 负对照：把 reserveDirectoryQuota 里的「在途预留」从判定里去掉
// （used+size > quota）⇒ 本用例必红（所有并发者都读到 used=0 ⇒ 全部放行）。
func TestReserveDirectoryQuota_ConcurrentNoOvercommit(t *testing.T) {
	svc, _, _, _, _ := newTestServiceWithDB(t)
	root := t.TempDir()
	const quota = 100
	const size = 30
	const n = 10

	var mu sync.Mutex
	var releases []func()
	okCount := 0
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			release, err := svc.reserveDirectoryQuota(context.Background(), root, quota, size)
			if err != nil {
				return
			}
			mu.Lock()
			okCount++
			releases = append(releases, release)
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	// 账本不残留（释放后归零）
	mu.Lock()
	defer mu.Unlock()
	for _, r := range releases {
		r()
	}

	assert.Equal(t, int64(quota/size), int64(okCount),
		"并发预留必须按在途字节计账（只允许 %d 笔 %d 字节）", quota/size, size)
	assert.LessOrEqual(t, int64(okCount)*size, int64(quota), "并发预留不得超放")
}

// TestImportLocalFile_DirectoryQuota_ConcurrentNoOvercommit 并发导入不得超放。
func TestImportLocalFile_DirectoryQuota_ConcurrentNoOvercommit(t *testing.T) {
	root := t.TempDir()
	const fileSize = 30
	const quota = 100
	paths := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		paths = append(paths, writeLocalFile(t, root, fmt.Sprintf("f%02d.txt", i), fileSize))
	}
	withDirectoryPolicy(t, root, quota, "allow")

	svc, gormDB, _, _, _ := newTestServiceWithDB(t)
	rootReal, rerr := filepath.EvalSymlinks(root)
	require.NoError(t, rerr)

	var wg sync.WaitGroup
	var mu sync.Mutex
	okCount := 0
	for _, p := range paths {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			if _, err := svc.ImportLocalFile(context.Background(), ImportLocalOpts{AbsPath: p}); err == nil {
				mu.Lock()
				okCount++
				mu.Unlock()
			}
		}(p)
	}
	wg.Wait()

	var total int64
	require.NoError(t, gormDB.Model(&model.FileCode{}).
		Where("dir_root = ?", rootReal).
		Select("COALESCE(SUM(size), 0)").Scan(&total).Error)
	assert.LessOrEqual(t, total, int64(quota), "并发导入的目录存活用量不得超配额")
	assert.LessOrEqual(t, int64(okCount)*fileSize, int64(quota))
}
