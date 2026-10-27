package admin

import "testing"

// TestNormalizeLogPageLocksCurrentSemantics 锁定系统日志分页口径的**现状**语义。
//
// 本域口径与全仓其它域**都不同**（默认 50、上限 500）⇒ 收编到
// oekit/pkg/pagination 后，期望值必须一字不改（响应条数对调用方可见）。
func TestNormalizeLogPageLocksCurrentSemantics(t *testing.T) {
	cases := []struct {
		name           string
		page, pageSize int
		wantPage       int
		wantPageSize   int
	}{
		{"默认页码", 0, 20, 1, 20},
		{"负页码", -3, 20, 1, 20},
		{"非法页长", 1, 0, 1, 50},
		{"负页长", 1, -5, 1, 50},
		{"等于上限", 1, 500, 1, 500},
		{"超上限截断", 1, 501, 1, 500},
		{"巨量页长截断", 1, 100000, 1, 500},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotPage, gotPageSize := normalizeLogPage(c.page, c.pageSize)
			if gotPage != c.wantPage || gotPageSize != c.wantPageSize {
				t.Fatalf("normalizeLogPage(%d, %d) = (%d, %d)，期望 (%d, %d)",
					c.page, c.pageSize, gotPage, gotPageSize, c.wantPage, c.wantPageSize)
			}
		})
	}
}
