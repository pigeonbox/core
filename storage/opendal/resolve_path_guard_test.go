// core#223：OpenDAL fs 驱动 resolvePath 包含性校验 + 未知 scheme 显式报错。
//
// 修复前 resolvePath 只是 filepath.Join(op.root, strings.TrimPrefix(p,"/"))：
//   - ".." 不设防 ⇒ 所有 fs 分支（Write/Read/Stat/Delete/RemoveAll/...）可跳出 root；
//   - 未知 scheme 静默回落 SchemeFS，Root 为空时落在进程 CWD。
//
// 本文件钉住：三条逃逸形态在每个 fs 分支都返回错误（不是静默拼路径），
// 未知 scheme / 空 Root 构造直接报错，正常相对路径不受影响。
package opendal

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newFSOperator(t *testing.T) (*Operator, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "root")
	require.NoError(t, os.MkdirAll(root, 0o755))
	op, err := New(Config{Scheme: SchemeFS, Root: root})
	require.NoError(t, err)
	return op, root
}

// TestFSOperatorNormalPathsStillWork 负对照：正常相对路径全部照常工作
// （守卫不能把合法路径一起打死）。
func TestFSOperatorNormalPathsStillWork(t *testing.T) {
	op, root := newFSOperator(t)
	ctx := context.Background()

	require.NoError(t, op.Write(ctx, "uploads/2026/a.txt", []byte("hello")))
	got, err := op.Read(ctx, "uploads/2026/a.txt")
	require.NoError(t, err)
	assert.Equal(t, "hello", string(got))
	md, err := op.Stat(ctx, "/uploads/2026/a.txt")
	require.NoError(t, err)
	assert.EqualValues(t, 5, md.Size)

	require.NoError(t, op.WriteStream(ctx, "uploads/b.bin", strings.NewReader("0123456789"), 10))
	require.NoError(t, op.CreateDir(ctx, "chunks/u1"))
	require.NoError(t, op.Copy(ctx, "uploads/2026/a.txt", "uploads/copied.txt"))
	require.NoError(t, op.Rename(ctx, "uploads/copied.txt", "uploads/renamed.txt"))
	items, err := op.List(ctx, "uploads/2026")
	require.NoError(t, err)
	require.Len(t, items, 1)

	require.NoError(t, op.Delete(ctx, "uploads/renamed.txt"))
	require.NoError(t, op.RemoveAll(ctx, "chunks/u1"))
	_, err = os.Stat(filepath.Join(root, "uploads/2026/a.txt"))
	require.NoError(t, err, "正常路径必须落在 root 内")
}

// TestFSOperatorRejectsTraversal 三条逃逸形态 × 全部分支。
func TestFSOperatorRejectsTraversal(t *testing.T) {
	escapes := []string{"../escaped", "/../escaped", "a/../../escaped"}
	for _, p := range escapes {
		p := p
		t.Run(p, func(t *testing.T) {
			op, root := newFSOperator(t)
			ctx := context.Background()
			outside := filepath.Join(filepath.Dir(root), "escaped")

			require.Error(t, op.Write(ctx, p, []byte("x")), "Write 必须拒绝越界")
			require.NoFileExists(t, outside, "越界写不得落盘")
			_, err := op.Read(ctx, p)
			require.Error(t, err, "Read 必须拒绝越界")
			_, err = op.Stat(ctx, p)
			require.Error(t, err, "Stat 必须拒绝越界")
			require.Error(t, op.Delete(ctx, p), "Delete 必须拒绝越界")
			require.Error(t, op.RemoveAll(ctx, p), "RemoveAll 必须拒绝越界")
			_, err = op.List(ctx, p)
			require.Error(t, err, "List 必须拒绝越界")
			require.Error(t, op.CreateDir(ctx, p), "CreateDir 必须拒绝越界")
			require.Error(t, op.Copy(ctx, p, "inside.txt"), "Copy 源越界必须拒绝")
			require.Error(t, op.Copy(ctx, "inside.txt", p), "Copy 目标越界必须拒绝")
			require.Error(t, op.Rename(ctx, p, "inside.txt"), "Rename 源越界必须拒绝")
			require.Error(t, op.Rename(ctx, "inside.txt", p), "Rename 目标越界必须拒绝")
			require.False(t, op.Exists(ctx, p), "Exists 对越界路径必须为 false")
		})
	}
}

// TestNewRejectsUnknownScheme 未知 scheme 不再静默回落 fs（宽容兜底已无收益）。
func TestNewRejectsUnknownScheme(t *testing.T) {
	_, err := New(Config{Scheme: Scheme("bogus"), Root: t.TempDir()})
	require.Error(t, err, "未知 scheme 必须报错，不得静默当 fs")
	assert.Contains(t, err.Error(), "bogus")
}

// TestNewFSRequiresRoot fs scheme 不允许空 Root（否则路径相对进程 CWD）。
func TestNewFSRequiresRoot(t *testing.T) {
	for _, root := range []string{"", "   "} {
		_, err := New(Config{Scheme: SchemeFS, Root: root})
		require.Errorf(t, err, "fs root=%q 必须报错（绝不落 CWD）", root)
	}
}
