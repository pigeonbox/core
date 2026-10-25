package resp

import (
	"testing"

	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/pigeonbox/contracts/errcode"
)

// 业务码 → HTTP 状态 的映射契约（2026-10-25 审计 A1/A9）。
//
// 缺陷：2xxxx 族（分享域业务码）此前**整族未枚举** ⇒ `httpStatusForCode` 一律落
// `default` ⇒ **HTTP 200**。后果：
//   - 「取件次数用完」在匿名通道（20007）是 200、在分享通道（20004，handler 显式
//     写 403）是 403 ⇒ **同一语义事件两个状态码**；
//   - 客户端若按 HTTP 状态分支（而非按响应体 `code`）会把「额度用完」当成功。
//
// 断言必须能变红（负对照）：把 2xxxx 的 case 从映射里去掉 ⇒ 本用例红。
func TestHTTPStatusForCode_2xxxxFamily(t *testing.T) {
	cases := []struct {
		name string
		code int
		want int
	}{
		{"分享不存在", errcode.CodeShareNotFound, consts.StatusNotFound},
		{"取件码不存在", errcode.CodePickupCodeNotFound, consts.StatusNotFound},
		{"分享过期", errcode.CodeShareExpired, consts.StatusNotFound},
		{"取件码已过期", errcode.CodePickupCodeExpired, consts.StatusNotFound},
		// 核心：两个通道的「次数用完」必须**同一个状态码**
		{"超过取件次数(分享通道)", errcode.CodeShareReachLimit, consts.StatusForbidden},
		{"取件码已用完(匿名通道)", errcode.CodePickupCodeExhausted, consts.StatusForbidden},
	}
	for _, c := range cases {
		if got := httpStatusForCode(c.code); got != c.want {
			t.Errorf("%s: httpStatusForCode(%d) = %d, want %d", c.name, c.code, got, c.want)
		}
	}
}

// TestHTTPStatusForCode_SameSemanticSameStatus 同一语义 ⇒ 同一 HTTP 状态。
//
// 这是 A1 的**不变量形式**（比逐码枚举更抗漂移）：「次数用完」的两个码
// （20004/20007）必须给出相同状态；「不存在/已过期」族必须都是 404。
func TestHTTPStatusForCode_SameSemanticSameStatus(t *testing.T) {
	groups := []struct {
		name  string
		codes []int
		want  int
	}{
		{"次数用完", []int{errcode.CodeShareReachLimit, errcode.CodePickupCodeExhausted}, consts.StatusForbidden},
		{"不存在/已过期", []int{
			errcode.CodeNotFound, errcode.CodeShareNotFound, errcode.CodePickupCodeNotFound,
			errcode.CodeShareExpired, errcode.CodePickupCodeExpired,
		}, consts.StatusNotFound},
	}
	for _, g := range groups {
		for _, c := range g.codes {
			if got := httpStatusForCode(c); got != g.want {
				t.Errorf("%s 族: httpStatusForCode(%d) = %d, want %d（族内必须同态）", g.name, c, got, g.want)
			}
		}
	}
}

// TestHTTPStatusForCode_No2xxxxFallsToOK 2xxxx 族**不得**再落 default(=200)：
// 那正是「同一语义两个状态码」的根源（未被显式枚举的码会静默变 200）。
func TestHTTPStatusForCode_No2xxxxFallsToOK(t *testing.T) {
	// 逐个枚举本仓 2xxxx 段已定义的码（新增码时同步补映射与本用例）
	known := []int{
		errcode.CodeShareNotFound, errcode.CodeShareExpired, errcode.CodeShareReachLimit,
		errcode.CodePickupCodeNotFound, errcode.CodePickupCodeExpired, errcode.CodePickupCodeExhausted,
	}
	for _, c := range known {
		if got := httpStatusForCode(c); got == consts.StatusOK {
			t.Errorf("业务码 %d 落到 HTTP 200（未显式枚举）——错误响应不该是 200", c)
		}
	}
}
