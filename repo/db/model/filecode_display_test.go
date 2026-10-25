package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestGetFilePath_SingleLayout 回归：file_path 口径统一为「完整相对路径」，
// GetFilePath 不再按「目录 + uuid 文件名」拼装（双重路径的根因）。
func TestGetFilePath_SingleLayout(t *testing.T) {
	// 完整路径 + 同名 uuid：取 file_path 本身（不重复拼文件名）
	fc := &FileCode{FilePath: "uploads/2026/10/25/x.txt", UUIDFileName: "x.txt"}
	assert.Equal(t, "uploads/2026/10/25/x.txt", fc.GetFilePath())

	// 完整路径 + 空 uuid：同值
	fc = &FileCode{FilePath: "uploads/2026/10/25/y.txt"}
	assert.Equal(t, "uploads/2026/10/25/y.txt", fc.GetFilePath())

	// 空路径：空串
	fc = &FileCode{UUIDFileName: "z.txt"}
	assert.Equal(t, "", fc.GetFilePath())
}

// TestDisplayName_OriginalNameFirst 回归：展示名口径统一为「原始文件名(Text) →
// 存储名(UUIDFileName) → 路径末段」——与子表 FileCodeFile.DisplayName 一致。
func TestDisplayName_OriginalNameFirst(t *testing.T) {
	// 原始名优先（此前是 UUIDFileName 优先 ⇒ 管理端列表与取件页两个名字）
	fc := &FileCode{
		FilePath:     "uploads/2026/10/25/56781532-5786-4724-a143-f9bd15b83e91.txt",
		UUIDFileName: "56781532-5786-4724-a143-f9bd15b83e91.txt",
		Text:         "m1.txt",
	}
	assert.Equal(t, "m1.txt", fc.DisplayName())

	// 无原始名：回退存储名
	fc = &FileCode{
		FilePath:     "uploads/2026/10/25/56781532-5786-4724-a143-f9bd15b83e91.txt",
		UUIDFileName: "56781532-5786-4724-a143-f9bd15b83e91.txt",
	}
	assert.Equal(t, "56781532-5786-4724-a143-f9bd15b83e91.txt", fc.DisplayName())

	// 两者皆无：回退路径末段
	fc = &FileCode{FilePath: "uploads/2026/10/25/up_abc.bin"}
	assert.Equal(t, "up_abc.bin", fc.DisplayName())

	// 纯文本分享：空串（正文不是文件名）
	fc = &FileCode{Text: "正文内容"}
	assert.True(t, fc.IsTextShare())
	assert.Equal(t, "", fc.DisplayName())

	// nil 安全
	var nilFC *FileCode
	assert.Equal(t, "", nilFC.DisplayName())
}
