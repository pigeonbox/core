package paging

import "testing"

// TestPoliciesKeepDistinctSemantics 钉住「各域口径**互不相同**」这一事实。
//
// 同一输入 pageSize=101，在各域得到**不同条数**：
//
//	notify（回落）              ⇒  20
//	mcp 分享列表（截断 100）    ⇒ 100
//	dao 用户分享（截断 100）    ⇒ 100
//	dao 通用（上限 200）        ⇒ 101（不超限，原样）
//	admin 操作审计（上限 200）  ⇒ 101
//	admin 系统日志（上限 500）  ⇒ 101
//	mcp 用户列表（不限上限）     ⇒ 101
//
// ⇒ 这不是「重复实现」，是**多个既有契约**（响应条数对消费方可见）。
// 谁若「顺手统一口径」（比如把 notify 的回落改成截断，或把 dao 通用上限
// 从 200 抬到 500），本测试立刻红。
func TestPoliciesKeepDistinctSemantics(t *testing.T) {
	const input = 101
	cases := []struct {
		name         string
		policy       interface{ Normalize(int, int) (int, int) }
		wantPageSize int
	}{
		{"notify（回落）", NotifyPolicy, 20},
		{"mcp 分享列表（截断 100）", MCPListSharesPolicy, 100},
		{"dao 用户分享（截断 100）", UserSharePolicy, 100},
		{"dao 通用（上限 200）", DAOPolicy, 101},
		{"admin 操作审计（上限 200）", AdminActivitiesPolicy, 101},
		{"admin 系统日志（上限 500）", AdminSystemLogPolicy, 101},
		{"mcp 用户列表（不限上限）", MCPListUsersPolicy, 101},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, gotPageSize := c.policy.Normalize(1, input)
			if gotPageSize != c.wantPageSize {
				t.Fatalf("pageSize=%d 归一化后 = %d，期望 %d（口径被改了？）",
					input, gotPageSize, c.wantPageSize)
			}
		})
	}
}

// TestPoliciesNormalizeInvalidValues 钉住「非法值归一化」在各口径上的一致形态：
// page ≤ 0 ⇒ 口径默认页码；pageSize ≤ 0 ⇒ 口径默认页长。
func TestPoliciesNormalizeInvalidValues(t *testing.T) {
	cases := []struct {
		name         string
		policy       interface{ Normalize(int, int) (int, int) }
		wantPage     int
		wantPageSize int
	}{
		{"notify", NotifyPolicy, 1, 20},
		{"mcp 分享列表", MCPListSharesPolicy, 1, 20},
		{"mcp 用户列表", MCPListUsersPolicy, 1, 20},
		{"dao 通用", DAOPolicy, 1, 20},
		{"dao 用户分享", UserSharePolicy, 1, 20},
		{"admin 系统日志", AdminSystemLogPolicy, 1, 50},
		{"admin 操作审计", AdminActivitiesPolicy, 1, 20},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotPage, gotPageSize := c.policy.Normalize(0, 0)
			if gotPage != c.wantPage || gotPageSize != c.wantPageSize {
				t.Fatalf("Normalize(0, 0) = (%d, %d)，期望 (%d, %d)",
					gotPage, gotPageSize, c.wantPage, c.wantPageSize)
			}
			gotPage, gotPageSize = c.policy.Normalize(-5, -5)
			if gotPage != c.wantPage || gotPageSize != c.wantPageSize {
				t.Fatalf("Normalize(-5, -5) = (%d, %d)，期望 (%d, %d)",
					gotPage, gotPageSize, c.wantPage, c.wantPageSize)
			}
		})
	}
}
