// 远端 presign 孤儿清理的**安全性**断言（2026-10-25 补零测试面时发现：
// app/admin 覆盖率仅 16.7%，而这里的决策是**破坏性**的 ⇒ 判据出错会删掉
// 在用文件）。
//
// 清理判据（app/admin/janitor.go ScanRemotePresignOrphans）：
//
//	① 看 `uploads/` 前缀全量对象（评审 D03 起不再限定 basename `up_`——
//	   uuid 命名的分片/直传对象在远端无 ReconcileOrphans 兜底，漏扫=永久泄漏）；
//	② **有 DB 引用**（file_codes.file_path + file_code_files.file_path，含软删行）
//	   ⇒ 不是孤儿；
//	③ 在宽限期内 ⇒ 不是孤儿（可能正在直传）。
//
// 其余 ⇒ 孤儿 ⇒ 被 DeleteFile 删掉。
//
// 断言必须能变红（负对照）：把「有 DB 引用」的判定去掉 ⇒ 引用中的对象被删 ⇒
// 用例红。
package admin

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/pigeonbox/core/repo/db/model"
	"github.com/pigeonbox/core/storage"
)

// remoteStorageStub 远端存储桩：列举固定对象 + 记录被删的 key。
type remoteStorageStub struct {
	storage.StorageInterface
	objs    []storage.RemoteObjectInfo
	deleted []string
	typ     storage.StorageType
	listErr error
}

func (s *remoteStorageStub) EffectiveType() storage.StorageType { return s.typ }
func (s *remoteStorageStub) DataPath() string                   { return "/tmp/stub" }
func (s *remoteStorageStub) ListRemoteObjects(_ context.Context, prefix string) ([]storage.RemoteObjectInfo, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.objs, nil
}
func (s *remoteStorageStub) DeleteFile(_ context.Context, key string) error {
	s.deleted = append(s.deleted, key)
	return nil
}

// newJanitorEnv 构造 janitor（内存 sqlite + 远端存储桩）。
func newJanitorEnv(t *testing.T, objs []storage.RemoteObjectInfo) (*Janitor, *remoteStorageStub) {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}, &model.FileCodeFile{},
		&model.TransferLog{}, &model.AdminOperationLog{}, &model.UploadChunk{}))
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	stub := &remoteStorageStub{objs: objs, typ: storage.StorageTypeS3}
	return NewJanitor(g, stub, 0), stub
}

func obj(key string, age time.Duration) storage.RemoteObjectInfo {
	return storage.RemoteObjectInfo{Key: key, Size: 10, ModTime: time.Now().Add(-age)}
}

// TestCleanRemotePresignOrphans_NeverDeletesReferencedObject 核心安全属性：
// **有 DB 引用**的 presign 对象（= 已完成直传的分享）**绝不能**被当孤儿删掉。
func TestCleanRemotePresignOrphans_NeverDeletesReferencedObject(t *testing.T) {
	const referenced = "uploads/2026/10/25/up_aaaa.bin"
	const orphan = "uploads/2026/10/25/up_bbbb.bin"

	j, stub := newJanitorEnv(t, []storage.RemoteObjectInfo{
		obj(referenced, 48*time.Hour), // 超过宽限期，但**有引用**
		obj(orphan, 48*time.Hour),     // 超过宽限期，无引用 ⇒ 真孤儿
	})
	// 有引用的分享行（正常分享的 file_path 指向该对象）
	require.NoError(t, j.fileCodeRepo.Create(context.Background(), &model.FileCode{
		Code: "SHARE01", FilePath: referenced, UUIDFileName: "up_aaaa.bin",
	}))

	removed, err := j.CleanRemotePresignOrphans(context.Background(), 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, removed, "只应删掉 1 个真孤儿")

	assert.NotContains(t, stub.deleted, referenced,
		"**有 DB 引用**的对象绝不能被删（= 已完成直传的分享会丢文件）")
	assert.Contains(t, stub.deleted, orphan, "无引用且超宽限期的孤儿应被删")
}

// TestCleanRemotePresignOrphans_GracePeriodKeepsInFlight 宽限期内的对象
// （可能正在直传）不得被删。
func TestCleanRemotePresignOrphans_GracePeriodKeepsInFlight(t *testing.T) {
	const inflight = "uploads/2026/10/25/up_cccc.bin"

	j, stub := newJanitorEnv(t, []storage.RemoteObjectInfo{obj(inflight, time.Minute)})

	removed, err := j.CleanRemotePresignOrphans(context.Background(), 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 0, removed, "宽限期内不得清理")
	assert.Empty(t, stub.deleted, "宽限期内不得删（可能正在直传）")
}

// 评审 D03③：uploads/ 下 uuid 命名的孤儿（分片/直传对象）也必须被扫出——
// 原 `up_` basename 过滤把它们漏成永久泄漏（远端后端无 ReconcileOrphans 兜底，
// 本地才有磁盘对账兜底）。宽限期 + 含软删引用集两道闸不变 ⇒ 误删风险与 up_ 同级。
//
// 取代原 NeverTouchesNonPresignObjects（其前提「非 up_ 永不扫」已被评审 D03
// 显式推翻——那正是永久泄漏的成因）。
func TestScanRemoteUploadOrphans_CoversUUIDObjects(t *testing.T) {
	const upOrphan = "uploads/2026/11/14/up_1111.ext"          // presign 残留（既有覆盖）
	const uuidOrphan = "uploads/2026/11/14/2222-3333-4444.ext" // 分片/直传对象（原漏）
	const fresh = "uploads/2026/11/14/aaaa-bbbb-cccc.ext"      // 宽限期内（不扫）
	const softRef = "uploads/2026/11/14/dddd-eeee-ffff.ext"    // 被软删分享引用（不扫）

	j, _ := newJanitorEnv(t, []storage.RemoteObjectInfo{
		obj(upOrphan, 48*time.Hour),
		obj(uuidOrphan, 48*time.Hour),
		obj(fresh, time.Minute),
		obj(softRef, 48*time.Hour),
	})
	ctx := context.Background()

	// 软删分享仍引用 softRef——引用集必须含软删行（回收站对象不算孤儿）
	uid := uint(5)
	require.NoError(t, j.fileCodeRepo.Create(ctx, &model.FileCode{
		Code: "SOFTREF01", UserID: &uid, FilePath: softRef, UUIDFileName: "ffff.ext",
	}))
	n, err := j.fileCodeRepo.BatchSoftDeleteByCodes(ctx, uid, []string{"SOFTREF01"})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	keys, err := j.ScanRemotePresignOrphans(ctx, 24*time.Hour)
	require.NoError(t, err)
	assert.Contains(t, keys, upOrphan, "presign 残留仍须扫出（既有覆盖不回归）")
	assert.Contains(t, keys, uuidOrphan, "uuid 命名的分片/直传孤儿必须扫出（评审 D03③）")
	assert.NotContains(t, keys, fresh, "宽限期内对象不得扫（可能正在直传）")
	assert.NotContains(t, keys, softRef, "软删行仍引用的对象不得扫（引用集含软删口径）")
	assert.Len(t, keys, 2, "只应有 2 个真孤儿")
}

// TestCleanRemotePresignOrphans_LocalBackendSkips 本地后端无 presign 孤儿语义
// （磁盘对账另有覆盖）⇒ 恒返回 0。
func TestCleanRemotePresignOrphans_LocalBackendSkips(t *testing.T) {
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(&model.FileCode{}))
	stub := &remoteStorageStub{typ: storage.StorageTypeLocal, objs: []storage.RemoteObjectInfo{
		obj("uploads/2026/10/25/up_dddd.bin", 72*time.Hour),
	}}
	j := NewJanitor(g, stub, 0)

	removed, err := j.CleanRemotePresignOrphans(context.Background(), 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 0, removed, "本地后端不应做远端 presign 孤儿清理")
	assert.Empty(t, stub.deleted)
}
