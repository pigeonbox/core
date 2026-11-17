package utils

// L8 · path_escape：落盘相对路径的生成（原始文件名不得影响路径结构）。
//
// 威胁：上传的原始文件名完全客户端可控。若落盘路径由原始名拼成
// （或扩展名里带着目录成分），攻击者可用 "../../etc/cron.d/x" 一类名字把文件
// 写到存储根之外，或用 "a/../b" 让两个不同上传落到同一路径（互相覆盖）。
//
// 现状口径：磁盘名一律 UUID（pkg/utils/randomcode.go NewUploadRelPath 与
// pkg/utils/upload_path.go UploadObjectRelPath），原始名只用于扩展名推导与
// 展示 ⇒ 结构上不可能被原始名控制。但这两个函数此前 0 覆盖——
// 有人把 uuidName 改成"原始名 + uuid"或直接拼 rel 时，没有测试会红。
//
// 判据：rel 恒为 uploads/YYYY/MM/DD/<uuid><ext> 五段；磁盘名不含分隔符、
// 不含 ".."；原始名不改变路径结构；同名两次生成不同路径（不互相覆盖）。

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// l8RelPathPattern 落盘相对路径的形态：uploads/<4位年>/<2位月>/<2位日>/<uuid><ext>
var l8RelPathPattern = regexp.MustCompile(
	"^uploads/\\d{4}/\\d{2}/\\d{2}/[0-9a-fA-F-]{36}(\\.[^/\\\\]*)?$")

// l8AssertNoEscape 断言路径结构合法且不逃逸 uploads 根。
func l8AssertNoEscape(t *testing.T, rel, diskName string) {
	t.Helper()
	assert.True(t, l8RelPathPattern.MatchString(rel),
		"落盘路径必须恒为 uploads/YYYY/MM/DD/<uuid><ext> 形态，实际: %s", rel)
	assert.NotContains(t, diskName, "/", "磁盘名不得含目录分隔符: %s", diskName)
	assert.NotContains(t, diskName, "\\", "磁盘名不得含目录分隔符: %s", diskName)
	assert.NotEqual(t, "..", diskName)

	clean := filepath.Clean(rel)
	assert.False(t, strings.HasPrefix(clean, ".."), "路径不得逃逸存储根: %s", rel)
	assert.True(t, strings.HasPrefix(clean, "uploads"+string(filepath.Separator)),
		"路径必须落在 uploads/ 下: %s", rel)

	// 逐段检查：不得出现 ".." 段
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		assert.NotEqual(t, "..", seg, "路径不得含上跳段: %s", rel)
	}
}

// TestL8_NewUploadRelPathIgnoresHostileFileName 恶意原始名不得影响落盘路径结构。
func TestL8_NewUploadRelPathIgnoresHostileFileName(t *testing.T) {
	for _, name := range []string{
		"../../etc/passwd",
		"..\\..\\windows\\system32\\evil.exe",
		"/etc/shadow",
		"a/../../b.txt",
		"..",
		".",
		"",
		"正常名字.txt",
	} {
		diskName, rel := NewUploadRelPath(name)
		require.NotEmpty(t, rel)
		l8AssertNoEscape(t, rel, diskName)
	}
}

// TestL8_NewUploadRelPathKeepsExtensionOnly 原始名只贡献扩展名。
func TestL8_NewUploadRelPathKeepsExtensionOnly(t *testing.T) {
	diskName, rel := NewUploadRelPath("report.txt")
	require.NotEmpty(t, diskName)
	assert.True(t, strings.HasSuffix(diskName, ".txt"), "扩展名应保留: %s", diskName)
	assert.True(t, l8RelPathPattern.MatchString(rel), "路径形态必须固定: %s", rel)

	// 原始名本体不得出现在磁盘名里
	assert.NotContains(t, diskName, "report")
}

// TestL8_UploadObjectRelPathIgnoresHostileFileName 直传/分片合并的落盘路径同口径。
func TestL8_UploadObjectRelPathIgnoresHostileFileName(t *testing.T) {
	for _, name := range []string{
		"../../etc/passwd",
		"..\\..\\evil.exe",
		"/abs/path.bin",
		"照片.jpg",
	} {
		rel := UploadObjectRelPath(name)
		require.NotEmpty(t, rel)
		diskName := filepath.Base(rel)
		l8AssertNoEscape(t, rel, diskName)
	}
}

// TestL8_UploadRelPathIsUniquePerCall 同名两次生成不得落到同一路径（互相覆盖）。
func TestL8_UploadRelPathIsUniquePerCall(t *testing.T) {
	n1, r1 := NewUploadRelPath("same.txt")
	n2, r2 := NewUploadRelPath("same.txt")
	assert.NotEqual(t, n1, n2, "同名两次必须生成不同磁盘名")
	assert.NotEqual(t, r1, r2, "同名两次必须生成不同路径")
}
