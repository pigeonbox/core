package security

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestContentDispositionAttachment(t *testing.T) {
	// 纯 ASCII 名原样输出，不带 filename*
	assert.Equal(t, `attachment; filename="report.pdf"`,
		ContentDispositionAttachment("report.pdf"))

	// 非 ASCII 名：filename= 纯 ASCII 兜底（保扩展名），filename* 承载原名
	got := ContentDispositionAttachment("报表.pdf")
	assert.Equal(t, `attachment; filename="download.pdf"; filename*=UTF-8''%E6%8A%A5%E8%A1%A8.pdf`, got)

	// 主干夹杂 ASCII 时保留可辨识片段，连续非 ASCII 折叠单个 '_'
	got = ContentDispositionAttachment("2026年度报告-v2.pdf")
	assert.Equal(t, `attachment; filename="2026_-v2.pdf"; filename*=UTF-8''2026%E5%B9%B4%E5%BA%A6%E6%8A%A5%E5%91%8A-v2.pdf`, got)

	// 无扩展名纯中文 → download
	assert.Contains(t, ContentDispositionAttachment("年度总结"), `filename="download"`)

	// 引号/控制字符消毒（防头断义/头注入）
	got = ContentDispositionAttachment("a\"b\r\nX.pdf")
	assert.NotContains(t, got, "\r")
	assert.NotContains(t, got, "\n")
	assert.NotContains(t, got, `"b`)
	assert.Contains(t, got, `filename="a_b__X.pdf"`)

	// RFC5987 attr-char 外字符逐字节编码：裸 ';' 会截断头参数，必须 %3B
	got = ContentDispositionAttachment("报告;v2.pdf")
	assert.Contains(t, got, `filename*=UTF-8''%E6%8A%A5%E5%91%8A%3Bv2.pdf`)
}
