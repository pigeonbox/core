package backup

// scheduler_diskfull_test.go —— 定时备份的 **写失败故障注入**（L6 故障注入）。
//
// 注入什么故障（两种形态，都是「写不进去」）：
//   ① 写盘中途失败：导出的对象流在拷贝途中报错（等价磁盘满 ENOSPC
//      写到一半）；
//   ② 临时文件建不出来：在「临时文件名」上先建一个**同名目录**
//      ⇒ os.OpenFile 落临时文件失败（EISDIR）——等价「目录不可写/权限不足」。
//      （不用 chmod：root 下 chmod 拦不住，判据会变成环境相关的假信号。）
//
// 断言什么（全部是**行为**，落地 app/backup/scheduler.go 的三条硬约束）：
//   ① **失败整体报错**：RunOnce 返回错误、结果为 nil（不是「半个成功」）。
//   ② **失败时旧包一个都不删**（keep=1 本会删掉它们）：失败路径不得做保留清理
//      ——「新包没成、旧包也没了」是灾备放大器。
//   ③ **不产生「看起来成功」的包**：目录里不得出现本次的成型包
//      （名字按 FileName(now) 判定），也不得残留 .partial 临时文件
//      （半个包会被当成「今天已备份」，当天真正的备份被跳过）。
//   ④ **失败在状态面可见**：Status().LastError 非空、LastFile 为空
//      （不得把失败记成某个成功包）。
//   ⑤ **失败后仍然到期重试**：Due() 仍为 true（今天没有成型包 ⇒ 下一轮补跑），
//      这正是「不把残骸记成备份成功」的可观测后果。
//
// 怎么让它变红（负对照，详见交付说明的实跑记录）：
//   - 把 runLocked 里的失败分支改成「照样执行 prune」⇒ ②红
//     （失败后删旧包 ⇒ 灾备放大器）；
//   - 把失败分支里的 os.Remove(tmpPath) 去掉 ⇒ ③红（残留半个包会被当成成品）；
//   - 把失败分支改成「记成功包（lastFile=包名）」⇒ ④红。
//
// 用例隔离：包内既有 newTestDB/fakeLister/newFakeStore/seedPkg 不动，
// 本文件用独立的 diskFullStore（读流中途失败）。

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/model"
)

// diskFullStore 读到一半就失败的对象存储（等价磁盘满写到一半）。
type diskFullStore struct {
	data map[string][]byte
	// failRead=true ⇒ GetFileReader 返回的流在读完一半后报错。
	failRead bool
}

func newDiskFullStore() *diskFullStore {
	return &diskFullStore{data: map[string][]byte{}}
}

func (s *diskFullStore) GetFileReader(_ context.Context, p string) (io.ReadCloser, int64, error) {
	data, ok := s.data[p]
	if !ok {
		return nil, 0, errors.New("object not found: " + p)
	}
	if s.failRead {
		// 声明的长度是真实长度，但流在中途出错 ⇒ 导出阶段的拷贝失败。
		return &halfFailReader{data: data}, int64(len(data)), nil
	}
	return io.NopCloser(newBytesReader(data)), int64(len(data)), nil
}

func (s *diskFullStore) EffectiveType() string { return "local" }

func (s *diskFullStore) SaveStream(_ context.Context, p string, r io.Reader, size int64) (int64, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	s.data[p] = b
	return int64(len(b)), nil
}

func (s *diskFullStore) FileExists(_ context.Context, p string) bool {
	_, ok := s.data[p]
	return ok
}

// halfFailReader 读到一半报错的流（模拟写盘失败）。
type halfFailReader struct {
	data []byte
	off  int
}

func (r *halfFailReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := len(p)
	if n > len(r.data)-r.off {
		n = len(r.data) - r.off
	}
	// 只放行一半，随后报「磁盘满」。
	if r.off+n > len(r.data)/2 {
		n = len(r.data)/2 - r.off
		if n <= 0 {
			return 0, errors.New("no space left on device")
		}
	}
	copy(p, r.data[r.off:r.off+n])
	r.off += n
	if r.off >= len(r.data)/2 {
		return n, errors.New("no space left on device")
	}
	return n, nil
}

func (r *halfFailReader) Close() error { return nil }

// newBytesReader 小工具（避免为一处引用引入 bytes 的额外别名）。
func newBytesReader(b []byte) io.Reader { return &sliceReader{b: b} }

type sliceReader struct {
	b   []byte
	off int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.off >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.off:])
	r.off += n
	return n, nil
}

// newDiskFullScheduler 建一个「写盘会失败」的调度器（keep=1：失败时若误做
// 保留清理，旧包会被删掉——正是要防的放大器）。
func newDiskFullScheduler(t *testing.T, now time.Time) (*Scheduler, string, *diskFullStore) {
	t.Helper()
	db := newTestDB(t, t.TempDir())
	seedFileCodes(t, db,
		model.FileCode{Code: "ABCD12", FilePath: "uploads/2026/11/06/a.txt", Size: 5, Status: "normal"},
	)
	store := newDiskFullStore()
	store.data["uploads/2026/11/06/a.txt"] = []byte("hello")
	store.failRead = true
	svc := NewService(db, fakeLister{paths: []string{"uploads/2026/11/06/a.txt"}}, store)

	dir := filepath.Join(t.TempDir(), "backups")
	sch, err := NewScheduler(svc, SchedulerConfig{Dir: dir, Keep: 1, At: "03:00"})
	require.NoError(t, err)
	sch.now = func() time.Time { return now }
	return sch, dir, store
}

// TestScheduler_DiskFullKeepsOldBackups 写盘中途失败 ⇒ 整体报错、旧包一个
// 都不删、不产生「看起来成功」的包、状态可见、仍然到期重试。
func TestScheduler_DiskFullKeepsOldBackups(t *testing.T) {
	now := time.Date(2026, 11, 6, 3, 0, 0, 0, time.Local)
	sch, dir, _ := newDiskFullScheduler(t, now)

	// 两份旧包（keep=1 ⇒ 成功路径本会删掉它们）。
	old := seedPkg(t, dir, time.Date(2026, 11, 4, 3, 0, 0, 0, time.Local))
	older := seedPkg(t, dir, time.Date(2026, 11, 3, 3, 0, 0, 0, time.Local))

	// ① 失败整体报错。
	res, err := sch.RunOnce(context.Background())
	require.Error(t, err, "写盘失败必须整体报错")
	assert.Nil(t, res, "失败不得返回一个「看起来成功」的结果对象")

	// ② 失败时旧包一个都不删。
	for _, path := range []string{old, older} {
		_, statErr := os.Stat(path)
		assert.NoError(t, statErr, "失败时不得删除旧备份（灾备放大器）: %s", path)
	}

	// ③ 不产生「看起来成功」的包：本次成型包不存在、也不残留 .partial。
	finalPath := filepath.Join(dir, FileName(now))
	_, statErr := os.Stat(finalPath)
	assert.True(t, os.IsNotExist(statErr), "失败不得留下成型包（会被当成今天已备份）: %s", finalPath)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), PartialSuffix, "失败后不得残留临时文件（半个包）")
	}

	// ④ 失败在状态面可见、且没被记成成功包。
	st := sch.Status()
	assert.NotEmpty(t, st.LastError, "失败原因必须在状态面可见")
	assert.Empty(t, st.LastFile, "失败不得把任何包记成成功包")
	assert.False(t, st.Running, "失败后必须解除 running")

	// ⑤ 失败后仍然到期重试（今天没有成型包）。
	due, err := sch.Due()
	require.NoError(t, err)
	assert.True(t, due, "失败后今天仍算「没备份」⇒ 下一轮必须补跑")
}

// TestScheduler_TempFileUnwritable_KeepsOldBackups 临时文件建不出来
// （目录不可写/权限不足的等价形态）：同样整体报错、旧包不删、不留半成品。
func TestScheduler_TempFileUnwritable_KeepsOldBackups(t *testing.T) {
	now := time.Date(2026, 11, 6, 3, 0, 0, 0, time.Local)
	sch, dir, _ := newDiskFullScheduler(t, now)

	old := seedPkg(t, dir, time.Date(2026, 11, 4, 3, 0, 0, 0, time.Local))
	older := seedPkg(t, dir, time.Date(2026, 11, 3, 3, 0, 0, 0, time.Local))

	// 在「临时文件名」上建一个同名目录 ⇒ 临时文件建不出来（EISDIR）。
	require.NoError(t, os.MkdirAll(filepath.Join(dir, FileName(now)+PartialSuffix), 0o700))

	res, err := sch.RunOnce(context.Background())
	require.Error(t, err, "临时文件建不出来必须整体报错")
	assert.Nil(t, res)
	assert.Contains(t, err.Error(), "创建临时备份文件失败", "错误必须点名失败点")

	// 旧包一个都不删。
	for _, path := range []string{old, older} {
		_, statErr := os.Stat(path)
		assert.NoError(t, statErr, "失败时不得删除旧备份: %s", path)
	}

	// 不产生成型包。
	_, statErr := os.Stat(filepath.Join(dir, FileName(now)))
	assert.True(t, os.IsNotExist(statErr), "失败不得留下成型包")

	// 状态可见。
	st := sch.Status()
	assert.NotEmpty(t, st.LastError)
	assert.Empty(t, st.LastFile, "失败不得把任何包记成成功包")
}

// TestScheduler_RecoveredAfterWriteFailure 写失败撤除后 ⇒ 同一目录下一轮
// 成功产出成型包（故障不毒化调度器），且此时才按保留策略清理旧包。
func TestScheduler_RecoveredAfterWriteFailure(t *testing.T) {
	now := time.Date(2026, 11, 6, 3, 0, 0, 0, time.Local)
	sch, dir, store := newDiskFullScheduler(t, now)

	old := seedPkg(t, dir, time.Date(2026, 11, 3, 3, 0, 0, 0, time.Local))

	// 失败一轮：旧包不动。
	_, err := sch.RunOnce(context.Background())
	require.Error(t, err)
	_, statErr := os.Stat(old)
	assert.NoError(t, statErr, "失败轮旧包必须仍在")

	// 撤除故障 ⇒ 下一轮成功（同一个调度器、同一目录）。
	store.failRead = false
	res, err := sch.RunOnce(context.Background())
	require.NoError(t, err, "存储恢复后下一轮必须成功")
	require.NotNil(t, res)
	assert.Equal(t, FileName(now), res.Name)
	assert.Greater(t, res.Size, int64(0), "产出的包不得是空壳")

	// 成型的包在、临时残骸不在。
	_, statErr = os.Stat(res.Path)
	assert.NoError(t, statErr)
	_, statErr = os.Stat(res.Path + PartialSuffix)
	assert.True(t, os.IsNotExist(statErr), "成功后不得残留 .partial")

	// 此时才按保留策略清理（keep=1 ⇒ 删掉更旧的那份）。
	assert.Len(t, res.Pruned, 1)
	_, statErr = os.Stat(old)
	assert.True(t, os.IsNotExist(statErr), "成功后按保留策略清理旧包")

	// 状态面：成功包被记住、失败原因被清空。
	st := sch.Status()
	assert.Equal(t, FileName(now), st.LastFile)
	assert.Empty(t, st.LastError, "成功一次即清空失败原因")
}
