package utils

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestValidateEmail 邮箱格式判据（core#119）。
// 负对照：把 ValidateEmail 的任一判据删掉（如控制字符循环 / 字面量全等）
// ⇒ 对应用例转红。
func TestValidateEmail(t *testing.T) {
	valid := []string{
		"user@example.com",
		"a.b+c@sub.domain.co",
		"x@y",            // 无 TLD 的本地域（RFC 合法）
		"用户@example.com", // UTF-8 本地部（国际化邮箱；无控制字符即无注入面）
	}
	for _, e := range valid {
		assert.NoError(t, ValidateEmail(e), "应接受: %q", e)
	}

	invalid := []string{
		"",
		"x@y.com\r\nBcc:evil@evil.com", // SMTP 头注入（issue 的守卫输入）
		"x@y.com\nBcc:evil",
		"x@y.com\r\n\r\nmail body",
		"x@y.com\x00",
		"x@y.com\tBcc:evil",
		"Name <x@y.com>", // display-name 形态（字面量 ≠ 地址）
		" x@y.com",       // 首尾空白
		"x@y.com ",
		"\"quoted\"@example.com", // 解析被归一化 ⇒ 字面量不全等
		"no-at-sign",
		"a@b@c.com",
		"x@y.com, z@w.com",
		strings.Repeat("a", 250) + "@example.com", // 超长
	}
	for _, e := range invalid {
		assert.Error(t, ValidateEmail(e), "应拒绝: %q", e)
	}
}
