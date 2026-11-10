// core#110：expire_value 解析双言——严格版 utils.ParseExpireParams 全仓零
// 生产调用（死代码），生产两处用 convert.Str2Int(.., 0) 宽松方言 ⇒ 非数字被
// 静默当 0（day ⇒ 静默改义 1 天、count ⇒ 0 = 建完即死且服务端回 success）。
//
// 本文件钉两件事：
//  1. ParseExpireParams 的严格语义（非数字/≤0 显式报错；forever 免 ≤0 检查）；
//  2. 生产调用点守卫（按调用点检查，防回退成 Str2Int 方言）。
//
// 负对照（已实跑）：把任一调用点换回 convert.Str2Int ⇒ 守卫用例转红；
// 把 ParseExpireParams 的报错分支改成静默归 0 ⇒ 严格语义用例转红。
package utils

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseExpireParams_Strict(t *testing.T) {
	ok := []struct{ value, style string }{
		{"1", "day"}, {"30", "day"}, {"3", "count"}, {"7", "week"},
		{"0", "forever"}, {"1", "forever"}, // forever 免 ≤0 检查
	}
	for _, tc := range ok {
		p, err := ParseExpireParams(tc.value, tc.style, "false")
		require.NoErrorf(t, err, "%s/%s 应放行", tc.value, tc.style)
		assert.Equal(t, tc.style, p.ExpireStyle)
	}
	bad := []struct{ value, style string }{
		{"abc", "day"},   // issue 点名形态：修前静默 1 天
		{"abc", "count"}, // issue 点名形态：修前 0=建完即死
		{"", "day"},      // 显式空值 = 解析失败（不是「没传」）
		{"1.5", "day"},
		{"-3", "day"},  // ≤0（forever 除外）显式报错
		{"0", "count"}, // count 0 = 建完即死，必须拒
		{"0", "day"},
		{"0", ""},          // 样式未指定时同样不接受 0
		{"abc", "forever"}, // forever 只免 ≤0 检查，不免非数字（严格版语义原样采纳）
	}
	for _, tc := range bad {
		_, err := ParseExpireParams(tc.value, tc.style, "false")
		require.Errorf(t, err, "%s/%s 应拒绝（修前静默改义/建完即死）", tc.value, tc.style)
	}
}

// TestParseExpireParamsHasProductionCallSites 守卫（core#110 修法要求）：
// ParseExpireParams 必须有生产调用点（按调用点检查，防回退成 Str2Int 方言）。
//
// 两处调用点即 issue 点名的生产方言所在地；Str2Int(expire_value) 形态不得回流。
func TestParseExpireParamsHasProductionCallSites(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	files := []string{
		"transport/http/handler/share_multi.go",
		"gen/handler/share_anonymous/share_anonymous_service.go",
	}
	forbidden := []string{
		`convert.Str2Int(c.DefaultPostForm("expire_value"`,
		`convert.Str2Int(req.GetExpireValue()`,
	}
	for _, f := range files {
		t.Run(f, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
			require.NoError(t, err)
			src := string(b)
			if !strings.Contains(src, "utils.ParseExpireParams(") {
				t.Errorf("%s 里找不到 utils.ParseExpireParams( ⇒ 严格收编被回退（core#110）", f)
			}
			for _, bad := range forbidden {
				if strings.Contains(src, bad) {
					t.Errorf("%s 里仍残留宽松方言 %q ⇒ 非数字会被静默当 0（core#110 要治的病）", f, bad)
				}
			}
		})
	}
}
