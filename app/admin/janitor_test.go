package admin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/repo/db/model"
	"github.com/pigeonbox/core/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func newJanitorTestEnv(t *testing.T) (string, *storage.StorageService, *gorm.DB) {
	t.Helper()
	root := t.TempDir()
	g, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, g.AutoMigrate(
		&model.FileCode{}, &model.UploadChunk{},
		&model.TransferLog{}, &model.AdminOperationLog{},
	))
	// glebarez/sqlite 的 :memory: 每条连接是独立库，多连接会拿到无表空库；
	// 钉死单连接消除该 flake（异步 goroutine 与主流程并发取连接时必现）。
	sqlDB, err := g.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	svc, err := storage.NewStorageServiceE(&storage.StorageConfig{
		Type:     storage.StorageTypeLocal,
		DataPath: root,
	})
	require.NoError(t, err)
	return root, svc, g
}

func TestReconcileOrphans(t *testing.T) {
	root, svc, g := newJanitorTestEnv(t)
	ctx := context.Background()

	// 被 DB 引用的文件（保留）
	keepRel := "uploads/2026/10/03/keep-uuid.txt"
	keepAbs := filepath.Join(root, filepath.FromSlash(keepRel))
	require.NoError(t, os.MkdirAll(filepath.Dir(keepAbs), 0o755))
	require.NoError(t, os.WriteFile(keepAbs, []byte("keep"), 0o644))
	require.NoError(t, g.Create(&model.FileCode{
		Code: "KEEPAAAA", FilePath: "uploads/2026/10/03", UUIDFileName: "keep-uuid.txt",
	}).Error)

	// 无引用的孤儿文件（删除）
	orphanAbs := filepath.Join(root, "uploads/2026/10/03/orphan-uuid.txt")
	require.NoError(t, os.WriteFile(orphanAbs, []byte("orphan"), 0o644))

	// 软删记录引用的文件（保留，可恢复）
	deletedRel := "uploads/2026/10/03/softdel-uuid.txt"
	softAbs := filepath.Join(root, filepath.FromSlash(deletedRel))
	require.NoError(t, os.WriteFile(softAbs, []byte("soft"), 0o644))
	require.NoError(t, g.Create(&model.FileCode{
		Code: "SOFTAAAA", FilePath: "uploads/2026/10/03", UUIDFileName: "softdel-uuid.txt",
	}).Error)
	require.NoError(t, g.Delete(&model.FileCode{}, "code = ?", "SOFTAAAA").Error)

	// 活跃分片目录（保留）与孤儿分片目录（删除）
	activeChunk := filepath.Join(root, "chunks", "active-upload")
	require.NoError(t, os.MkdirAll(activeChunk, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(activeChunk, "0.part"), []byte("c"), 0o644))
	require.NoError(t, g.Create(&model.UploadChunk{
		UploadID: "active-upload", ChunkIndex: -1,
	}).Error)
	orphanChunk := filepath.Join(root, "chunks", "orphan-upload")
	require.NoError(t, os.MkdirAll(orphanChunk, 0o755))

	// mtime 宽限（24h）下的对账只清"足够老"的孤儿——把孤儿文件/目录时间回拨 48h
	old := time.Now().Add(-48 * time.Hour)
	past := func(paths ...string) {
		for _, p := range paths {
			require.NoError(t, os.Chtimes(p, old, old))
		}
	}
	past(orphanAbs, orphanChunk)

	j := NewJanitor(g, svc, 90)
	scanned, removed, err := j.ReconcileOrphans(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, scanned, 2)
	assert.Equal(t, 2, removed) // 孤儿文件 + 孤儿分片目录

	_, err = os.Stat(keepAbs)
	assert.NoError(t, err, "被引用文件应保留")
	_, err = os.Stat(orphanAbs)
	assert.True(t, os.IsNotExist(err), "孤儿文件应删除")
	_, err = os.Stat(softAbs)
	assert.NoError(t, err, "软删记录引用的文件应保留")
	_, err = os.Stat(activeChunk)
	assert.NoError(t, err, "活跃分片目录应保留")
	_, err = os.Stat(orphanChunk)
	assert.True(t, os.IsNotExist(err), "孤儿分片目录应删除")

	// 新鲜孤儿（mtime 在宽限期内）放过不删
	freshAbs := filepath.Join(root, "uploads/2026/10/03/fresh-orphan.txt")
	require.NoError(t, os.WriteFile(freshAbs, []byte("fresh"), 0o644))
	_, removed2, err := j.ReconcileOrphans(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, removed2, "宽限期内的文件不得删除")
	_, err = os.Stat(freshAbs)
	assert.NoError(t, err, "新鲜孤儿应保留")
}

func TestCleanupLogs(t *testing.T) {
	_, svc, g := newJanitorTestEnv(t)
	ctx := context.Background()

	old := time.Now().AddDate(0, 0, -30)
	// go1.26 不支持提升字段（gorm.Model.CreatedAt）结构体字面量，先建后回填时间
	lg1 := &model.TransferLog{Operation: "upload"}
	require.NoError(t, g.Create(lg1).Error)
	require.NoError(t, g.Model(lg1).UpdateColumn("created_at", old).Error)
	lg2 := &model.AdminOperationLog{Action: "x"}
	require.NoError(t, g.Create(lg2).Error)
	require.NoError(t, g.Model(lg2).UpdateColumn("created_at", old).Error)
	lg3 := &model.TransferLog{Operation: "upload"}
	require.NoError(t, g.Create(lg3).Error)

	j := NewJanitor(g, svc, 7)
	n, err := j.CleanupLogs(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)

	var cnt int64
	require.NoError(t, g.Model(&model.TransferLog{}).Count(&cnt).Error)
	assert.Equal(t, int64(1), cnt, "只删超过保留期的")

	// 0 = 永久保留
	j0 := NewJanitor(g, svc, 0)
	n, err = j0.CleanupLogs(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)
}
