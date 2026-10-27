package mcp

import "testing"

// TestNormalizeMcpPageLocksCurrentSemantics 锁定 MCP 两个列表工具的分页口径现状。
//
// ⚠️ 两个工具口径**不同**，且用户列表**不设上限**：
//
//	pageSize=101：分享列表 ⇒ 100（截断）；用户列表 ⇒ 101（不限上限，原样）
//
// ⇒ 「顺手统一」会让其中一个工具的返回条数变掉（对 AI 工具调用方可见）。
func TestNormalizeMcpPageLocksCurrentSemantics(t *testing.T) {
	shares := []struct {
		name           string
		page, pageSize int
		wantPage       int
		wantPageSize   int
	}{
		{"默认页码", 0, 20, 1, 20},
		{"负页码", -3, 20, 1, 20},
		{"非法页长", 1, 0, 1, 20},
		{"负页长", 1, -5, 1, 20},
		{"等于上限", 1, 100, 1, 100},
		{"超上限截断", 1, 101, 1, 100},
		{"巨量页长截断", 1, 100000, 1, 100},
	}
	for _, c := range shares {
		t.Run("分享列表/"+c.name, func(t *testing.T) {
			gotPage, gotPageSize := normalizeListSharesPage(c.page, c.pageSize)
			if gotPage != c.wantPage || gotPageSize != c.wantPageSize {
				t.Fatalf("normalizeListSharesPage(%d, %d) = (%d, %d)，期望 (%d, %d)",
					c.page, c.pageSize, gotPage, gotPageSize, c.wantPage, c.wantPageSize)
			}
		})
	}

	users := []struct {
		name           string
		page, pageSize int
		wantPage       int
		wantPageSize   int
	}{
		{"默认页码", 0, 20, 1, 20},
		{"负页码", -3, 20, 1, 20},
		{"非法页长", 1, 0, 1, 20},
		{"负页长", 1, -5, 1, 20},
		{"不设上限原样", 1, 101, 1, 101},
		{"巨量页长原样", 1, 100000, 1, 100000},
	}
	for _, c := range users {
		t.Run("用户列表/"+c.name, func(t *testing.T) {
			gotPage, gotPageSize := normalizeListUsersPage(c.page, c.pageSize)
			if gotPage != c.wantPage || gotPageSize != c.wantPageSize {
				t.Fatalf("normalizeListUsersPage(%d, %d) = (%d, %d)，期望 (%d, %d)",
					c.page, c.pageSize, gotPage, gotPageSize, c.wantPage, c.wantPageSize)
			}
		})
	}
}
