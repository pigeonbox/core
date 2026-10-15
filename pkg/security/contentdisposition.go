package security

import (
	"fmt"
	"strings"
)

// ContentDispositionAttachment 构造下载 Content-Disposition（RFC 6266 + 5987）：
//   - 引号/反斜杠/控制字符消毒防头断义；
//   - 纯 ASCII 名只给 filename=；
//   - 非 ASCII 名（中文等）补 filename*=UTF-8”<pct-encoded>，且 filename= 改用
//     纯 ASCII 兜底名——老客户端（Android 9 下载器等）不认 filename*，遇到裸
//     UTF-8 字节会把整个头丢弃变成 download.bin，兜底名至少保住扩展名与可辨识度。
//
// pct-encoded 按 RFC 5987 attr-char 白名单逐字节编码（PathEscape 会放行
// `;`/`,` 等非 attr-char，ext-value 里裸分号会截断头参数，故不复用它）。
func ContentDispositionAttachment(name string) string {
	safe := sanitizeDispositionName(name)
	if isASCIIPrintable(safe) {
		return fmt.Sprintf(`attachment; filename="%s"`, safe)
	}
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`,
		asciiFallbackName(safe), rfc5987Encode(safe))
}

// sanitizeDispositionName 去掉引号/反斜杠/控制字符（含 CR/LF，防头注入）。
func sanitizeDispositionName(name string) string {
	return strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < 0x20 || r == 0x7f {
			return '_'
		}
		return r
	}, name)
}

func isASCIIPrintable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return s != ""
}

// asciiFallbackName 生成纯 ASCII 兜底名：ASCII 字符保留、非 ASCII 折叠为
// 单个 '_'，保留 ASCII 扩展名，主干全灭时退化为 "download"。
func asciiFallbackName(name string) string {
	stem, ext := name, ""
	if i := strings.LastIndexByte(name, '.'); i > 0 && isASCIIPrintable(name[i+1:]) && len(name)-i-1 <= 16 {
		stem, ext = name[:i], name[i:]
	}
	var b strings.Builder
	prevUnderscore := false
	for _, r := range stem {
		switch {
		case r >= 0x20 && r <= 0x7e:
			b.WriteRune(r)
			prevUnderscore = false
		case !prevUnderscore:
			b.WriteByte('_')
			prevUnderscore = true
		}
	}
	fallback := strings.Trim(b.String(), "_ ")
	if fallback == "" {
		fallback = "download"
	}
	return fallback + ext
}

// isRFC5987AttrChar RFC 5987 attr-char = ALPHA / DIGIT / !#$&+-.^_`|~
func isRFC5987AttrChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("!#$&+-.^_`|~", c) >= 0
}

func rfc5987Encode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if c := s[i]; isRFC5987AttrChar(c) {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
