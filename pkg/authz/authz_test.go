package authz

import "testing"

// TestRoleJudgments 角色判定核契约：admin 字面量单点 + 在职组合 + Require 系判定。
func TestRoleJudgments(t *testing.T) {
	cases := []struct {
		name string
		got  bool
		want bool
	}{
		{"admin 角色", IsAdminRole("admin"), true},
		{"user 角色", IsAdminRole("user"), false},
		{"空角色", IsAdminRole(""), false},
		{"大小写敏感（Admin 不是 admin）", IsAdminRole("Admin"), false},

		{"在职管理员", IsActiveAdmin("admin", "active"), true},
		{"降权后非 admin", IsActiveAdmin("user", "active"), false},
		{"禁用的 admin", IsActiveAdmin("admin", "disabled"), false},
		{"空角色空状态", IsActiveAdmin("", ""), false},

		{"角色满足", RoleSatisfies("user", "user"), true},
		{"角色不满足", RoleSatisfies("user", "admin"), false},
		{"空角色不满足非空所需", RoleSatisfies("", "user"), false},

		{"任一角色命中", AnyRoleSatisfies("admin", "user", "admin"), true},
		{"任一角色未命中", AnyRoleSatisfies("guest", "user", "admin"), false},
		{"空允许集恒不满足", AnyRoleSatisfies("admin"), false},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %v want %v", c.name, c.got, c.want)
		}
	}
}

// TestOwnedByUser 归属判定核契约：nil 双向不匹配 + 值相等才归。
func TestOwnedByUser(t *testing.T) {
	u := func(v uint) *uint { return &v }
	cases := []struct {
		name         string
		owner, callr *uint
		want         bool
	}{
		{"同一用户", u(7), u(7), true},
		{"不同用户", u(7), u(8), false},
		{"属主 nil（匿名资源）不归登录用户", nil, u(7), false},
		{"调用方 nil（未登录）不归任何人", u(7), nil, false},
		{"双 nil 不匹配", nil, nil, false},
		{"零值指针指向 0 归 0", u(0), u(0), true},
	}
	for _, c := range cases {
		if got := OwnedByUser(c.owner, c.callr); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// TestOwnedByIP 匿名归属契约：空属主 IP 恒不匹配（防空串互等打穿修复）。
func TestOwnedByIP(t *testing.T) {
	cases := []struct {
		name, owner, callr string
		want               bool
	}{
		{"同 IP", "10.0.0.1", "10.0.0.1", true},
		{"不同 IP", "10.0.0.1", "10.0.0.2", false},
		{"属主空恒不匹配（即使调用方也空）", "", "", false},
		{"属主空、调用方有值", "", "10.0.0.1", false},
		{"属主有值、调用方空", "10.0.0.1", "", false},
	}
	for _, c := range cases {
		if got := OwnedByIP(c.owner, c.callr); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
