package notify

import (
	"testing"

	"github.com/pigeonbox/core/internal/paging"
)

// TestNormalizeNotifyPageLocksCurrentSemantics 锁定 notify 域分页口径的**现状**语义。
//
// ⚠️ 本域的超限语义是**回落**（pageSize>100 ⇒ 回退默认 20），
// 而 dao 域是**截断**（pageSize>上限 ⇒ 钳到上限）⇒ 两者响应条数不同。
//
// 迁移到 oekit/pkg/pagination 时，最容易出的错就是把这个 Fallback 写成 Clamp
// （或反过来）——那时**线上响应条数会变**，而两边的单测都还是绿的
// （各自只测自己那套）⇒ 必须在这里把「回落」的期望值写死。
func TestNormalizeNotifyPageLocksCurrentSemantics(t *testing.T) {
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
		{"等于上限", 1, 100, 1, 100},
		{"超上限回落", 1, 101, 1, 20},
		{"巨量页长回落", 1, 100000, 1, 20},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotPage, gotPageSize := paging.NotifyPolicy.Normalize(c.page, c.pageSize)
			if gotPage != c.wantPage || gotPageSize != c.wantPageSize {
				t.Fatalf("Normalize(%d, %d) = (%d, %d)，期望 (%d, %d)",
					c.page, c.pageSize, gotPage, gotPageSize, c.wantPage, c.wantPageSize)
			}
		})
	}
}
