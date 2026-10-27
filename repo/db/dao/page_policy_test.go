package dao

import (
	"testing"

	"github.com/oekit/pkg/pagination"
	"github.com/pigeonbox/core/internal/paging"
)

// TestNormalizePageLocksCurrentSemantics 锁定 dao 侧**两套**分页口径的现状语义。
//
// 迁移到 oekit/pkg/pagination 后，本用例打 policy（期望值一字不改）⇒
// 迁移前后「响应条数」必然一致（页长决定返回条数，条数变了就是线上行为变了）。
//
// 为什么要锁：**同一个 pageSize 超限，在本仓有多种可观测的响应条数**——
//
//	pageSize=101：
//	  dao 通用口径（上限 200）⇒ 101（不超限，原样）
//	  用户分享口径（上限 100）⇒ 100（截断）
//	  notify 口径（上限 100）⇒  20（回落默认，见 app/notify 的用例）
//
// 三者**不能顺手统一**（前端 / MCP 消费方按域各自适应）⇒ 迁移只把口径变成
// 声明式，口径本身一字不改。
func TestNormalizePageLocksCurrentSemantics(t *testing.T) {
	cases := []struct {
		name           string
		policy         pagination.Policy
		page, pageSize int
		wantPage       int
		wantPageSize   int
	}{
		{"默认页码", paging.DAOPolicy, 0, 20, 1, 20},
		{"负页码", paging.DAOPolicy, -3, 20, 1, 20},
		{"非法页长", paging.DAOPolicy, 1, 0, 1, 20},
		{"负页长", paging.DAOPolicy, 1, -5, 1, 20},
		{"等于上限", paging.DAOPolicy, 1, 200, 1, 200},
		{"超上限截断", paging.DAOPolicy, 1, 201, 1, 200},
		{"巨量页长截断", paging.DAOPolicy, 1, 100000, 1, 200},
		{"用户分享等于上限", paging.UserSharePolicy, 1, 100, 1, 100},
		{"用户分享超上限截断", paging.UserSharePolicy, 1, 101, 1, 100},
		{"用户分享巨量页长截断", paging.UserSharePolicy, 1, 100000, 1, 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotPage, gotPageSize := c.policy.Normalize(c.page, c.pageSize)
			if gotPage != c.wantPage || gotPageSize != c.wantPageSize {
				t.Fatalf("Normalize(%d, %d) = (%d, %d)，期望 (%d, %d)",
					c.page, c.pageSize, gotPage, gotPageSize, c.wantPage, c.wantPageSize)
			}
		})
	}
}
