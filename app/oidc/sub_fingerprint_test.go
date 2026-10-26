package oidc

import (
	"strings"
	"testing"
)

// 回归（2026-10-25 审计 A8）：`claims.Sub[:8]` 切片越界 panic。
//
// sub 的唯一约束是**非空**（见 service.go 的 "userinfo 缺少 sub" 检查），
// **长度不保证** ⇒ IdP 返回长度 < 8 的 sub（如 "a1"）⇒ panic ⇒ 该 IdP 的
// 所有 OIDC 登录 500（hertz Recovery 只把它转成 500，不救功能）。
//
// 断言必须能变红（负对照）：把 oidcSubFingerprint 改回 `sub[:8]` ⇒ 本用例红。
func TestOIDCSubFingerprint_NoPanicOnShortSub(t *testing.T) {
	// 各种长度（含 < 8 的边界）都不得 panic，且结果非空
	for _, sub := range []string{"a", "ab", "abc", "abcdefg", "abcdefgh", "abcdefghi", strings.Repeat("x", 512)} {
		got := oidcSubFingerprint(sub)
		if got == "" {
			t.Errorf("sub=%q 指纹为空", sub)
		}
		if len(got) != 16 {
			t.Errorf("sub=%q 指纹长度=%d，期望 16（固定宽度才可做邮箱占位）", sub, len(got))
		}
	}
}

// 指纹必须**稳定**（同 sub 每次一致 ⇒ 重试建号不产生第二个用户）。
//
// ⚠️ 原实现写成 `oidcSubFingerprint(sub) != oidcSubFingerprint(sub)`
// ⇒ **左右是同一个表达式** ⇒ 恒为 false ⇒ 这条断言**永远不会失败**
// （golangci-lint 的 SA4000 正是为此报的警）。⇒ 改成「两次调用的结果相比」，
// 并额外**钉住具体取值**：指纹是 sha256 前 8 字节的十六进制，跨版本必须一致，
// 否则升级后同一个 IdP 账号会被认成新用户（重试建号撞唯一索引）。
func TestOIDCSubFingerprint_Stable(t *testing.T) {
	sub := "user-0001-abcdef"
	first := oidcSubFingerprint(sub)
	second := oidcSubFingerprint(sub)
	if first != second {
		t.Errorf("同 sub 的指纹必须稳定，实际 %q != %q", first, second)
	}
	if want := "f66cca5650818d94"; first != want {
		t.Errorf("指纹取值漂移：实际 %q，期望 %q（算法/宽度变了 ⇒ 存量 IdP 账号会被认成新用户）", first, want)
	}
}

// 指纹必须**不碰撞**：旧实现取前 8 位 ⇒ 前 8 位相同的两个 sub 会撞
// 用户名后缀与占位邮箱 ⇒ 后者建号失败（唯一索引冲突）。
func TestOIDCSubFingerprint_NoCollisionOnSharedPrefix(t *testing.T) {
	a := oidcSubFingerprint("user-0001-aaa")
	b := oidcSubFingerprint("user-0001-bbb")
	if a == b {
		t.Errorf("前 8 位相同的两个 sub 指纹碰撞了：%s（建号会撞唯一索引）", a)
	}
	// 前 8 位完全相同但更长的 sub 也不该碰撞
	c := oidcSubFingerprint("user-0001-aaabbbccc")
	d := oidcSubFingerprint("user-0001-aaabbbddd")
	if c == d {
		t.Errorf("长 sub 前缀相同仍碰撞：%s", c)
	}
}
