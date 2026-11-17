package backup

// export_backend_down_test.go —— 备份导出时的 **存储后端故障注入**（L6 故障注入）。
//
// 注入什么故障：
//   导出期间对象存储后端不可达（等价远端 503 / 网络断）。
//
// 背景（为什么这条值得单列）：
//   备份域把「对象本来就不存在」与「存储后端出故障」当成同一件事——
//   app/backup/export.go:148 用「if !s.store.FileExists(ctx, p)」判断「存量缺失」，
//   而存储层的 FileExists（storage/storage.go:FileExists →
//   opendal.Operator.Exists）在**任何**错误上都返回 false（Stat 失败即 false）。
//   ⇒ 后端一挂，全部对象都被判成「本来就不存在」⇒ 导出**照样成功**、
//   包里 objects/ 目录是空的、manifest 里挂着一长串 MissingObjects。
//
//   这与 export.go 自己的注释直接冲突（同文件 141-149 行：「而『存储后端真出
//   故障』仍会报错（下面的读失败分支）⇒ 不吞真故障」）——读失败分支只在对象
//   **被判定为存在**时才会走到；后端挂掉时压根走不到。
//
//   后果：灾备在最需要它的时候产出一个**看着成功、实际缺对象**的包；
//   恢复后这些分享指向的对象不在包里 ⇒ 静默的数据缺口。
//
// 断言什么：
//   ① 后端不可达 ⇒ 导出**必须失败**（不得产出「看起来成功」的包）；
//   ② 真·对象缺失（后端正常、文件确实不在）⇒ 仍按既有语义记 MissingObjects
//      并**成功**导出（这是另一回事，不能一起改掉）。
//
// 怎么让它变红（负对照，详见交付说明的实跑记录）：
//   - 把 export.go 的 FileExists 判断去掉（改成直接读、读失败即报错）
//     ⇒ ①红（当前的「吞真故障」行为被暴露出来）；
//   - 反过来把「真缺失」也改成报错 ⇒ ②红（既有语义被破坏）。
//
// 最小修法建议（不在本批次落地——本文件只加测试，不改生产代码）：
//   ObjectStore 接口加一个可返回错误的存在性查询（如
//   StatObject(ctx, path) (size int64, exists bool, err error)），
//   Export 用它区分「不存在」(exists=false, err=nil ⇒ 记 MissingObjects)
//   与「后端故障」(err≠nil ⇒ 整体报错)；storage.StorageService 侧用它替换
//   吞错的 FileExists 即可（契约层不动）。

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/model"
)

// downBackendStore 模拟「后端不可达」的对象存储。
//
// 形态取自生产链路：storage.StorageService.FileExists 在 Stat 失败时返回
// false（不区分「不存在」与「后端挂了」），而 GetFileReader 会如实报后端错误。
type downBackendStore struct {
	data map[string][]byte
	down bool
}

func (s *downBackendStore) GetFileReader(_ context.Context, p string) (io.ReadCloser, int64, error) {
	if s.down {
		return nil, 0, errors.New("backend unavailable: 503 Service Unavailable")
	}
	d, ok := s.data[p]
	if !ok {
		return nil, 0, errors.New("object not found: " + p)
	}
	return io.NopCloser(newBytesReader(d)), int64(len(d)), nil
}

func (s *downBackendStore) EffectiveType() string { return "local" }

func (s *downBackendStore) SaveStream(_ context.Context, p string, r io.Reader, _ int64) (int64, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	s.data[p] = b
	return int64(len(b)), nil
}

func (s *downBackendStore) FileExists(_ context.Context, p string) bool {
	if s.down {
		return false // 后端不可达 ⇒ 存储层的 Exists 恒 false（吞错）
	}
	_, ok := s.data[p]
	return ok
}

// ObjectExists 错误可见的存在性探测（**修复后**的存储层形态）：
// 后端不可达 ⇒ **如实报错**（不吞成「对象不存在」）；对象真不在 ⇒ (false, nil)；
// 在 ⇒ (true, nil)。
//
// 这正是本次修复新增的能力：存储层的 FileExists 吞错、ObjectExists 不吞。
func (s *downBackendStore) ObjectExists(_ context.Context, p string) (bool, error) {
	if s.down {
		return false, errors.New("backend unavailable: 503 Service Unavailable")
	}
	_, ok := s.data[p]
	return ok, nil
}

// TestExport_BackendDown_MustFailNotSilentlyOmitObjects 后端不可达 ⇒ 导出
// 必须失败（不得产出「看起来成功、实际缺对象」的包）。
func TestExport_BackendDown_MustFailNotSilentlyOmitObjects(t *testing.T) {
	db := newTestDB(t, t.TempDir())
	seedFileCodes(t, db,
		model.FileCode{Code: "ABCD12", FilePath: "uploads/2026/11/06/a.txt", Size: 5, Status: "normal"},
	)
	store := &downBackendStore{data: map[string][]byte{}, down: true}
	svc := NewService(db, fakeLister{paths: []string{"uploads/2026/11/06/a.txt"}}, store)

	manifest, err := svc.Export(context.Background(), io.Discard)
	require.Error(t, err,
		"存储后端不可达时导出必须失败（不得产出缺对象的「成功」包——灾备缺口会静默扩散）")
	assert.Nil(t, manifest, "失败不得同时返回一个成功清单")
}

// TestExport_ObjectGenuinelyMissing_StillRecorded 真·对象缺失（后端正常、
// 文件确实不在）⇒ 仍按既有语义记 MissingObjects 并**成功**导出。
//
// 这是上一条的对照组：保证「后端故障 ⇒ 报错」这条修法不会把「存量缺失」
// 一起改成失败（真机 2026-10-31 事故：300+ 条分享记录里若干对象已被清理，
// 「读不到就整体失败」⇒ 备份包根本导不出来 ⇒ 灾备在最需要时失效）。
func TestExport_ObjectGenuinelyMissing_StillRecorded(t *testing.T) {
	db := newTestDB(t, t.TempDir())
	seedFileCodes(t, db,
		model.FileCode{Code: "ABCD12", FilePath: "uploads/2026/11/06/gone.txt", Size: 5, Status: "normal"},
	)
	// 后端正常，但对象确实不存在（store.data 为空）。
	store := &downBackendStore{data: map[string][]byte{}}
	svc := NewService(db, fakeLister{paths: []string{"uploads/2026/11/06/gone.txt"}}, store)

	manifest, err := svc.Export(context.Background(), io.Discard)
	require.NoError(t, err, "真·存量缺失不得让整体导出失败（既有语义）")
	require.NotNil(t, manifest)
	assert.Len(t, manifest.MissingObjects, 1, "存量缺失必须如实记账")
	assert.Equal(t, "uploads/2026/11/06/gone.txt", manifest.MissingObjects[0])
	assert.Zero(t, manifest.ObjectCount, "缺失对象不计入已备份对象数")
}
