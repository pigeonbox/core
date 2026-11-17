package backup

// export_probe_test.go —— 存在性探测的**两条路径**（能力在 / 能力不在）。
//
// 背景：备份域必须分清「对象真的不存在」与「存储后端出故障」——
//   · 存储层给了 ObjectExistsProbe（错误可见的探测）⇒ 直接用它的答案；
//   · 没给（装配方只转发既有 4 个方法 ⇒ **生产装配当前就是这个形态**）
//     ⇒ 兜底走「读一次看错误」。
//
// 断言（全部是行为）：
//   ① 没有探测能力时，后端不可达 ⇒ 导出**仍必须失败**（兜底路径不得退化成吞错）；
//   ② 没有探测能力时，真·对象缺失 ⇒ 仍记 missing 且导出成功（既有语义不丢）；
//   ③ 探测能力报错 ⇒ 导出失败且**不返回**成功清单；
//   ④ 探测能力优先于吞错的 FileExists（探测说「在」⇒ 对象必须真的进包）。
//
// 负对照：把 Service.objectExists 的错误上抛改成吞错（把 err 当成「不存在」）
// ⇒ ①③红。

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pigeonbox/core/repo/db/model"
)

// probeLessStore **没有** ObjectExistsProbe 能力的对象存储。
//
// 形态 = 装配方只转发既有 4 个方法时（bootstrap 的 storageTypeAdapter 就是
// 这个形态）⇒ 导出走「读一次看错误」的兜底路径。
type probeLessStore struct {
	data map[string][]byte
	// down=true ⇒ 后端不可达（读直接失败，且 FileExists 吞错恒 false）。
	down bool
	// lieExists=true ⇒ FileExists 一律说「不在」（模拟存储层吞错）。
	lieExists bool
}

func (s *probeLessStore) GetFileReader(_ context.Context, p string) (io.ReadCloser, int64, error) {
	if s.down {
		return nil, 0, errors.New("backend unavailable: 503 Service Unavailable")
	}
	d, ok := s.data[p]
	if !ok {
		// 真·缺失 ⇒ 存储层契约：包装 os.ErrNotExist（见 storage/BEHAVIOR-MATRIX.md）。
		return nil, 0, os.ErrNotExist
	}
	return io.NopCloser(newBytesReader(d)), int64(len(d)), nil
}

func (s *probeLessStore) EffectiveType() string { return "local" }

func (s *probeLessStore) SaveStream(_ context.Context, p string, r io.Reader, _ int64) (int64, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	s.data[p] = b
	return int64(len(b)), nil
}

// FileExists 吞错（生产链路的真实形态）：后端不可达 ⇒ 恒 false。
func (s *probeLessStore) FileExists(_ context.Context, p string) bool {
	if s.down || s.lieExists {
		return false
	}
	_, ok := s.data[p]
	return ok
}

// TestExport_BackendDown_WithoutProbe_FallbackStillFails 没有探测能力时，
// 后端不可达 ⇒ 导出**仍必须失败**（兜底路径不得退化成吞错）。
func TestExport_BackendDown_WithoutProbe_FallbackStillFails(t *testing.T) {
	db := newTestDB(t, t.TempDir())
	seedFileCodes(t, db,
		model.FileCode{Code: "ABCD12", FilePath: "uploads/2026/11/06/a.txt", Size: 5, Status: "normal"},
	)
	store := &probeLessStore{data: map[string][]byte{}, down: true}
	svc := NewService(db, fakeLister{paths: []string{"uploads/2026/11/06/a.txt"}}, store)

	manifest, err := svc.Export(context.Background(), io.Discard)
	require.Error(t, err, "没有探测能力时后端故障也必须让导出失败（兜底路径不得吞错）")
	assert.Nil(t, manifest, "失败不得同时返回一个成功清单")
}

// TestExport_MissingObject_WithoutProbe_FallbackRecordsMissing 没有探测能力时，
// 真·对象缺失 ⇒ 仍按既有语义记 MissingObjects 并成功导出。
func TestExport_MissingObject_WithoutProbe_FallbackRecordsMissing(t *testing.T) {
	db := newTestDB(t, t.TempDir())
	seedFileCodes(t, db,
		model.FileCode{Code: "ABCD12", FilePath: "uploads/2026/11/06/gone.txt", Size: 5, Status: "normal"},
	)
	store := &probeLessStore{data: map[string][]byte{}}
	svc := NewService(db, fakeLister{paths: []string{"uploads/2026/11/06/gone.txt"}}, store)

	manifest, err := svc.Export(context.Background(), io.Discard)
	require.NoError(t, err, "真·存量缺失不得让整体导出失败（既有语义）")
	require.NotNil(t, manifest)
	require.Len(t, manifest.MissingObjects, 1)
	assert.Equal(t, "uploads/2026/11/06/gone.txt", manifest.MissingObjects[0])
	assert.Zero(t, manifest.ObjectCount, "缺失对象不计入已备份对象数")
}

// errProbeStore 探测能力**总是报错**的对象存储。
type errProbeStore struct{ probeLessStore }

func (s *errProbeStore) ObjectExists(_ context.Context, _ string) (bool, error) {
	return false, errors.New("backend unavailable: 503 Service Unavailable")
}

// TestExport_ProbeError_FailsExport 探测能力报错 ⇒ 导出失败且不返回成功清单。
func TestExport_ProbeError_FailsExport(t *testing.T) {
	db := newTestDB(t, t.TempDir())
	seedFileCodes(t, db,
		model.FileCode{Code: "ABCD12", FilePath: "uploads/2026/11/06/a.txt", Size: 5, Status: "normal"},
	)
	store := &errProbeStore{probeLessStore: probeLessStore{data: map[string][]byte{}}}
	svc := NewService(db, fakeLister{paths: []string{"uploads/2026/11/06/a.txt"}}, store)

	manifest, err := svc.Export(context.Background(), io.Discard)
	require.Error(t, err, "探测能力报错必须让导出失败")
	assert.Nil(t, manifest, "失败不得同时返回一个成功清单")
}

// truthProbeStore FileExists 说「不在」（吞错形态），探测能力说真话。
type truthProbeStore struct {
	probeLessStore
	exists bool
}

func (s *truthProbeStore) ObjectExists(_ context.Context, _ string) (bool, error) {
	return s.exists, nil
}

// TestExport_ProbeWinsOverSwallowingFileExists 探测能力优先于吞错的
// FileExists：探测说「在」⇒ 对象必须真的进包（不被假否定带偏）。
func TestExport_ProbeWinsOverSwallowingFileExists(t *testing.T) {
	db := newTestDB(t, t.TempDir())
	seedFileCodes(t, db,
		model.FileCode{Code: "ABCD12", FilePath: "uploads/2026/11/06/a.txt", Size: 5, Status: "normal"},
	)
	store := &truthProbeStore{
		probeLessStore: probeLessStore{
			data:      map[string][]byte{"uploads/2026/11/06/a.txt": []byte("hello")},
			lieExists: true,
		},
		exists: true,
	}
	svc := NewService(db, fakeLister{paths: []string{"uploads/2026/11/06/a.txt"}}, store)

	manifest, err := svc.Export(context.Background(), io.Discard)
	require.NoError(t, err)
	require.NotNil(t, manifest)
	assert.Equal(t, int64(1), manifest.ObjectCount, "探测说「在」⇒ 对象必须进包")
	assert.Empty(t, manifest.MissingObjects)
}
