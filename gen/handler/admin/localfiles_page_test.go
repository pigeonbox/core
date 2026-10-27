package admin

import "testing"

// TestNormalizeActivitiesPageLocksCurrentSemantics 锁定管理操作审计分页口径的现状语义。
func TestNormalizeActivitiesPageLocksCurrentSemantics(t *testing.T) {
	cases := []struct {
		name           string
		page, pageSize int
		wantPage       int
		wantPageSize   int
	}{
		{"默认页码", 0, 20, 1, 20},
		{"负页码", -3, 20, 1, 20},
		{"非法页长", 1, 0, 1, 20},
		{"负页长", 1, -5, 1, 20},
		{"等于上限", 1, 200, 1, 200},
		{"超上限截断", 1, 201, 1, 200},
		{"巨量页长截断", 1, 100000, 1, 200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotPage, gotPageSize := normalizeActivitiesPage(c.page, c.pageSize)
			if gotPage != c.wantPage || gotPageSize != c.wantPageSize {
				t.Fatalf("normalizeActivitiesPage(%d, %d) = (%d, %d)，期望 (%d, %d)",
					c.page, c.pageSize, gotPage, gotPageSize, c.wantPage, c.wantPageSize)
			}
		})
	}
}
