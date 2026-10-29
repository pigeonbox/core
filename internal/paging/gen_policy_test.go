package paging

import "testing"

// TestGenPoliciesMatchLegacyBehavior 钉住 #8 收编的**核心纪律**：收编只改「口径
// 怎么表达」，**口径本身一字不改** —— 迁移前后每个输入的归一化结果必须逐条一致。
//
// #8 收编前，这 4 处是**手写代码寄居在生成物文件里**（gen/handler 下）：
//
//	gen/handler/admin/admin_service.go  AdminListFiles   page≤0⇒1 / pageSize≤0⇒20，无上限
//	gen/handler/admin/admin_service.go  AdminListUsers   同上
//	gen/handler/user/user_service.go    UserFiles        page<1⇒1 / pageSize<1 **或>100**⇒20（回落）
//	gen/handler/share/share_service.go  UserSharesList   page<1⇒1 / pageSize<1⇒20，无上限
//
// 下表逐条照抄那 4 处的**原写法**算出的期望值 ⇒ 谁若顺手改了口径（把回落改成
// 截断、把不限上限改成有上限、把默认值从 20 改成别的），本用例立刻红。
//
// ⚠️ 注意 user 那条是**回落**（101 ⇒ 20）而不是截断（101 ⇒ 100）⇒ 与 dao 域、
// mcp 域同输入**不同条数** —— 这是**既有契约**（响应条数对消费方可见），不是
// 「重复实现」，不可顺手统一。
func TestGenPoliciesMatchLegacyBehavior(t *testing.T) {
	type legacy struct {
		name   string
		policy interface{ Normalize(int, int) (int, int) }
		// legacy 是该处**收编前**的归一化算法（逐条照抄原写法）。
		legacy func(page, pageSize int) (int, int)
	}
	cases := []legacy{
		{
			name:   "gen admin 列表（page≤0⇒1 / pageSize≤0⇒20 / 无上限）",
			policy: GenAdminListPolicy,
			legacy: func(page, pageSize int) (int, int) {
				if page <= 0 {
					page = 1
				}
				if pageSize <= 0 {
					pageSize = 20
				}
				return page, pageSize
			},
		},
		{
			name:   "gen user 文件列表（page<1⇒1 / pageSize<1 或>100⇒20 回落）",
			policy: GenUserFilesPolicy,
			legacy: func(page, pageSize int) (int, int) {
				if page < 1 {
					page = 1
				}
				if pageSize < 1 || pageSize > 100 {
					pageSize = 20
				}
				return page, pageSize
			},
		},
		{
			name:   "gen share 用户分享（page<1⇒1 / pageSize<1⇒20 / 无上限）",
			policy: GenShareUserSharesPolicy,
			legacy: func(page, pageSize int) (int, int) {
				if page < 1 {
					page = 1
				}
				if pageSize < 1 {
					pageSize = 20
				}
				return page, pageSize
			},
		},
	}

	inputs := []struct {
		page, pageSize int
	}{
		{1, 20}, {1, 1}, {1, 19}, {1, 21}, {1, 99}, {1, 100}, {1, 101},
		{1, 200}, {1, 500}, {1, 1000}, {0, 20}, {0, 0}, {-1, -1}, {2, 50},
		{3, 100}, {0, 101}, {-3, 500},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, in := range inputs {
				wantPage, wantPageSize := c.legacy(in.page, in.pageSize)
				gotPage, gotPageSize := c.policy.Normalize(in.page, in.pageSize)
				if gotPage != wantPage || gotPageSize != wantPageSize {
					t.Errorf("Normalize(%d, %d) = (%d, %d)，收编前的写法得 (%d, %d) —— 口径被改了？",
						in.page, in.pageSize, gotPage, gotPageSize, wantPage, wantPageSize)
				}
			}
		})
	}
}

// TestGenPoliciesKeepDistinctSemantics 钉住「#8 收编进来的这 3 个口径**互不相同**，
// 且与既有 7 个口径**也互不相同**」。
//
// 同一输入 pageSize=101，各域得到不同条数（响应条数对消费方可见 ⇒ 是既有契约）：
//
//	notify（回落）              ⇒  20
//	gen user 文件列表（回落）    ⇒  20   ← 与 notify 同值，但**独立声明**（各自可演进）
//	mcp 分享列表（截断 100）    ⇒ 100
//	dao 用户分享（截断 100）    ⇒ 100
//	dao 通用（上限 200）        ⇒ 101
//	admin 操作审计（上限 200）  ⇒ 101
//	admin 系统日志（上限 500）  ⇒ 101
//	mcp 用户列表（不限上限）     ⇒ 101
//	gen admin 列表（不限上限）   ⇒ 101
//	gen share 用户分享（不限）   ⇒ 101
//
// ⚠️ 「同值」不等于「同一口径」：回落组（notify / gen user）与不限组（mcp users /
// gen admin / gen share）各自的语义来源不同（不同域的既有契约）⇒ 声明必须**各写
// 一份**，不得合并成一个共享常量（合并后改一个就静默改掉另一个）。
func TestGenPoliciesKeepDistinctSemantics(t *testing.T) {
	const input = 101
	cases := []struct {
		name         string
		policy       interface{ Normalize(int, int) (int, int) }
		wantPageSize int
	}{
		{"notify（回落）", NotifyPolicy, 20},
		{"gen user 文件列表（回落 100）", GenUserFilesPolicy, 20},
		{"mcp 分享列表（截断 100）", MCPListSharesPolicy, 100},
		{"dao 用户分享（截断 100）", UserSharePolicy, 100},
		{"dao 通用（上限 200）", DAOPolicy, 101},
		{"admin 操作审计（上限 200）", AdminActivitiesPolicy, 101},
		{"admin 系统日志（上限 500）", AdminSystemLogPolicy, 101},
		{"mcp 用户列表（不限上限）", MCPListUsersPolicy, 101},
		{"gen admin 列表（不限上限）", GenAdminListPolicy, 101},
		{"gen share 用户分享（不限上限）", GenShareUserSharesPolicy, 101},
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

// TestGenPoliciesAreNotSharedDeclarations 钉住「gen 组三个口径是**三份独立声明**」，
// 而不是一个共享常量。
//
// 为什么钉这个：若有人图省事把 gen admin 与 gen share 都指向同一个 var
// （两者当前口径相同：20 / 不限上限）⇒ 将来改其中一个会**静默改掉**另一个 ⇒
// 两个域的响应条数一起变 ⇒ 无声回归。
//
// ⇒ 检测手段：三个 var 的**地址**必须互不相同（Go 里 var 各有独立地址）。
func TestGenPoliciesAreNotSharedDeclarations(t *testing.T) {
	if &GenAdminListPolicy == &GenUserFilesPolicy {
		t.Error("GenAdminListPolicy 与 GenUserFilesPolicy 指向同一处声明 ⇒ 改一个会静默改掉另一个")
	}
	if &GenAdminListPolicy == &GenShareUserSharesPolicy {
		t.Error("GenAdminListPolicy 与 GenShareUserSharesPolicy 指向同一处声明 ⇒ 改一个会静默改掉另一个")
	}
	if &GenUserFilesPolicy == &GenShareUserSharesPolicy {
		t.Error("GenUserFilesPolicy 与 GenShareUserSharesPolicy 指向同一处声明 ⇒ 改一个会静默改掉另一个")
	}
}
