package logger

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetGlobals 隔离 logger 包全局态（Logger/once 惰性守卫），用例结束还原。
// 只做赋值不拷贝 sync.Once（copylocks）。
func resetGlobals(t *testing.T) {
	t.Helper()
	old := Logger
	Logger = nil
	once = sync.Once{}
	t.Cleanup(func() {
		Logger = old
		once = sync.Once{}
	})
}

// TestOpenFileSink_NoRotationKeepsAppendPath MaxSize 未配置（=0，零配置默认态）
// 必须保持历史 O_APPEND 裸文件行为：返回 *os.File，不引入轮转语义。
func TestOpenFileSink_NoRotationKeepsAppendPath(t *testing.T) {
	dir := t.TempDir()
	filename := filepath.Join(dir, "app.log")

	sink, err := openFileSink(&Config{Filename: filename})
	require.NoError(t, err)

	f, isFile := sink.(*os.File)
	assert.True(t, isFile, "MaxSize=0 应保持裸 os.File（O_APPEND 路径），实际 %T", sink)

	_, err = f.Write([]byte("hello\n"))
	require.NoError(t, err)
	require.NoError(t, f.Sync())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "无轮转配置不应产生备份文件")

	data, err := os.ReadFile(filename)
	require.NoError(t, err)
	assert.Equal(t, "hello\n", string(data))
}

// TestOpenFileSink_RotatesWhenMaxSizeSet 显式配置 MaxSize 后轮转必须真实触发：
// 写超后目录出现备份文件（轮转修复前四字段空转，文件只增不减）。
func TestOpenFileSink_RotatesWhenMaxSizeSet(t *testing.T) {
	dir := t.TempDir()
	filename := filepath.Join(dir, "app.log")

	sink, err := openFileSink(&Config{
		Filename: filename,
		MaxSize:  1, // 1MB，分块累计写超以触发轮转
	})
	require.NoError(t, err)

	blob := make([]byte, 200*1024) // 200KB/笔（lumberjack 拒绝单笔超限写入，分块累计写超）
	for i := range blob {
		blob[i] = 'a'
	}
	for i := 0; i < 8; i++ { // 累计 1.6MB > MaxSize=1MB，中途必然轮转
		_, err = sink.Write(blob)
		require.NoError(t, err)
	}
	require.NoError(t, sink.Sync())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(entries), 2, "写超 MaxSize 后应出现备份/新文件，实际仅 %d 个", len(entries))

	// 主文件仍在位（lumberjack 轮转后重开同名文件继续写）
	_, err = os.Stat(filename)
	require.NoError(t, err)
}

// TestInit_WiresRotationSink Init 全链路冒烟：轮转配置下 Init 成功且日志落盘。
func TestInit_WiresRotationSink(t *testing.T) {
	resetGlobals(t)
	dir := t.TempDir()

	require.NoError(t, Init(&Config{Filename: filepath.Join(dir, "app.log"), MaxSize: 1}))
	Info("rotate smoke")
	Sync()

	data, err := os.ReadFile(filepath.Join(dir, "app.log"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "rotate smoke")
}
