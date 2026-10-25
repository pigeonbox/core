package utils

import "strings"

// TextPreview 取文本的一行预览（换行/竖线归一为空格，截断到 32 字符 + 省略号）。
//
// 用途：**文本分享的展示名**——文本分享没有文件名，取件页展示的就是这段预览
// （原先靠 Redis「展示信息快照」携带，2026-10-25 删除该快照后改为从 DB 的
// text 字段现算 ⇒ 两处共用本函数，保证「铸造时算的」与「取件时算的」一致）。
func TextPreview(text string) string {
	preview := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '|' {
			return ' '
		}
		return r
	}, text)
	runes := []rune(strings.TrimSpace(preview))
	if len(runes) > 32 {
		runes = append(runes[:32], []rune("…")...)
	}
	return string(runes)
}
