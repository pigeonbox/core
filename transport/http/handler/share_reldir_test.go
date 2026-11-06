package handler

// share_reldir_test.go —— 目录上传（5.3.0）的入参消毒守卫。
//
// rel_dir 来自浏览器的 webkitRelativePath，**完全客户端可控**，且最终会进 zip
// 条目名 ⇒ 放过一个 ".." 就是 Zip Slip（解压时文件被写到压缩包外的目录）。
// 路径穿越的四个经典形态（上跳段 / 绝对路径 / Windows 盘符 / UNC）逐个钉住。

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSanitizeShareRelDir_Accepts(t *testing.T) {
	cases := map[string]string{
		"":          "",
		"   ":       "",
		"a":         "a",
		"a/b":       "a/b",
		"a/b/c":     "a/b/c",
		"a\\b":      "a/b", // Windows 分隔符归一
		"./a/./b":   "a/b", // 当前目录段丢弃
		"a//b":      "a/b", // 重复分隔符折叠
		"a/b/":      "a/b", // 结尾斜杠不参与语义
		"照片/2024 春": "照片/2024 春",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			got, err := sanitizeShareRelDir(in)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

func TestSanitizeShareRelDir_Rejects(t *testing.T) {
	cases := map[string]string{
		"上跳段":        "../etc",
		"中层上跳段":      "a/../../b",
		"绝对路径":       "/etc/passwd",
		"前导斜杠":       "/a/b", // 前导 "/" 就是绝对路径：浏览器给的相对路径不会这样开头
		"Windows 盘符": "C:\\Windows",
		"UNC 路径":     "\\\\host\\share",
		"超长路径":       strings.Repeat("a", maxShareRelDirRunes+1),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := sanitizeShareRelDir(in)
			require.Error(t, err, "必须拒绝：%s", in)
			assert.Contains(t, err.Error(), "目录路径", "错误文案要能自证是哪一类问题")
		})
	}
}

// 长度边界：恰好等于上限放行（不能把边界值判成超限）
func TestSanitizeShareRelDir_LengthBoundary(t *testing.T) {
	ok := strings.Repeat("a", maxShareRelDirRunes)
	got, err := sanitizeShareRelDir(ok)
	require.NoError(t, err)
	assert.Equal(t, ok, got)
}
