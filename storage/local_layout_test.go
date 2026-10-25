package storage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateLegacyLayout_MovesUpOneLevel 回归：历史双层布局
// （<root>/uploads/uploads/…）上提一层到 <root>/uploads/…，旧目录随后删除。
func TestMigrateLegacyLayout_MovesUpOneLevel(t *testing.T) {
	root := t.TempDir()
	legacyFile := filepath.Join(root, "uploads", "uploads", "2026", "10", "25", "x.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(legacyFile), 0o755))
	require.NoError(t, os.WriteFile(legacyFile, []byte("payload"), 0o644))

	moved, skipped, err := MigrateLegacyLayout(root)
	require.NoError(t, err)
	assert.Equal(t, 1, moved)
	assert.Equal(t, 0, skipped)

	newPath := filepath.Join(root, "uploads", "2026", "10", "25", "x.txt")
	data, err := os.ReadFile(newPath)
	require.NoError(t, err, "文件应上提到单层布局")
	assert.Equal(t, "payload", string(data))

	_, err = os.Stat(filepath.Join(root, "uploads", "uploads"))
	assert.True(t, os.IsNotExist(err), "迁移后旧目录应被删除")
}

// TestMigrateLegacyLayout_NoopWhenAbsent 回归：没有历史布局时是 no-op（不报错、
// 不动任何文件）——绝大多数部署走的是这条路径。
func TestMigrateLegacyLayout_NoopWhenAbsent(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "uploads", "2026"), 0o755))
	keep := filepath.Join(root, "uploads", "2026", "keep.txt")
	require.NoError(t, os.WriteFile(keep, []byte("x"), 0o644))

	moved, skipped, err := MigrateLegacyLayout(root)
	require.NoError(t, err)
	assert.Equal(t, 0, moved)
	assert.Equal(t, 0, skipped)

	_, err = os.Stat(keep)
	assert.NoError(t, err, "既有文件不受影响")
}

// TestMigrateLegacyLayout_Idempotent 回归：重复执行不产生副作用。
func TestMigrateLegacyLayout_Idempotent(t *testing.T) {
	root := t.TempDir()
	legacyFile := filepath.Join(root, "uploads", "uploads", "a.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(legacyFile), 0o755))
	require.NoError(t, os.WriteFile(legacyFile, []byte("a"), 0o644))

	moved, _, err := MigrateLegacyLayout(root)
	require.NoError(t, err)
	assert.Equal(t, 1, moved)

	moved, skipped, err := MigrateLegacyLayout(root)
	require.NoError(t, err)
	assert.Equal(t, 0, moved)
	assert.Equal(t, 0, skipped)
}

// TestMigrateLegacyLayout_SkipOnConflict 回归：目标同名已存在时**跳过**（绝不覆盖
// 已有文件），旧项保留待人工处理。
func TestMigrateLegacyLayout_SkipOnConflict(t *testing.T) {
	root := t.TempDir()
	legacyFile := filepath.Join(root, "uploads", "uploads", "a.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(legacyFile), 0o755))
	require.NoError(t, os.WriteFile(legacyFile, []byte("legacy"), 0o644))

	target := filepath.Join(root, "uploads", "a.txt")
	require.NoError(t, os.WriteFile(target, []byte("current"), 0o644))

	moved, skipped, err := MigrateLegacyLayout(root)
	require.NoError(t, err)
	assert.Equal(t, 0, moved)
	assert.Equal(t, 1, skipped)

	data, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "current", string(data), "既有文件不得被覆盖")

	_, err = os.Stat(legacyFile)
	assert.NoError(t, err, "冲突项留在旧位置待人工处理")
}

// TestResolveLocal_SingleCandidate 回归：读路径只有「DataPath+rel」一条候选
// （历史回退分支已退役）。
func TestResolveLocal_SingleCandidate(t *testing.T) {
	root := t.TempDir()
	svc := NewLocalFallback(root, "")

	// 规范布局：<root>/uploads/2026/... 命中
	rel := "uploads/2026/10/25/x.txt"
	require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, rel), []byte("x"), 0o644))
	abs, ok := svc.resolveLocal(rel)
	assert.True(t, ok)
	assert.Equal(t, filepath.Join(root, rel), abs)

	// 历史双层布局不再回退（迁移会在启动期把它上提）
	legacyRel := "uploads/other/y.txt"
	require.NoError(t, os.MkdirAll(filepath.Join(root, "uploads", "uploads", "other"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "uploads", "uploads", "other", "y.txt"), []byte("y"), 0o644))
	_, ok = svc.resolveLocal(legacyRel)
	assert.False(t, ok, "历史回退分支已退役")
}
