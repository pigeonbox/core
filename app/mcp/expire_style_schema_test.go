package mcp

import (
	"reflect"
	"strings"
	"testing"

	"github.com/pigeonbox/core/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExpireStyleJsonschemaCoversAllStyles 钉住「MCP 工具的过期样式说明覆盖全部合法样式」。
//
// 2026-10-22 实弹：MCP 的 normalizeExpireStyle 早已用 pkg/utils 的单一真相源校验
// （count 可用），但**三处 jsonschema 描述**只写了旧的 7 个样式（其中一处连 forever
// 都没有）⇒ agent 看 schema 不知道有 count ⇒ **这个能力对 agent 不可见**（MCP 是
// 本产品对外的主入口之一，agent 只能凭 schema 选参数）。
//
// jsonschema 是**编译期常量**（struct tag），没法调函数生成 ⇒ 只能靠机械互锁：
// 真相源新增样式而 schema 没跟上 ⇒ 本用例即红。
func TestExpireStyleJsonschemaCoversAllStyles(t *testing.T) {
	// 收集所有工具入参里 expire_style 字段的 jsonschema 描述
	var texts []string
	for _, v := range []any{shareTextIn{}, shareFileIn{}} {
		tp := reflect.TypeOf(v)
		f, ok := tp.FieldByName("ExpireStyle")
		require.True(t, ok, "入参缺 ExpireStyle 字段")
		tag := f.Tag.Get("jsonschema")
		require.NotEmpty(t, tag, "ExpireStyle 缺 jsonschema 描述")
		texts = append(texts, tag)
	}
	require.GreaterOrEqual(t, len(texts), 2, "未收集到足够多的 expire_style schema")

	all := utils.AllExpireStyles()
	for _, style := range all {
		for i, txt := range texts {
			assert.Contains(t, txt, style,
				"第 %d 处 expire_style 的 jsonschema 漏了合法样式 %q（真相源 AllExpireStyles）", i+1, style)
		}
	}
	// 反向：schema 不得出现真相源之外的样式（防写错名字）
	for i, txt := range texts {
		// 取描述里 "过期样式：a/b/c，默认 day" 的 a/b/c 部分
		head := strings.TrimSpace(strings.Split(txt, "（")[0])
		head = strings.TrimPrefix(head, "过期样式：")
		head = strings.Split(head, "，")[0]
		for _, tok := range strings.Split(head, "/") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			assert.True(t, utils.IsValidExpireStyle(tok),
				"第 %d 处 expire_style 的 jsonschema 出现非法样式 %q", i+1, tok)
		}
	}
}
