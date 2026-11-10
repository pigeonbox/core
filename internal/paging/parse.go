// parse.go 分页参数的**严格解析**（core#104）。
//
// ⚠️ 这一层划清「解析」与「归一化」的职责：
//
//	解析（本文件）：query 字符串 → int —— **取值不合法 ⇒ 显式失败**。
//	归一化（policy.go 的各 Policy）：int → 该域合法页码/页长 —— **口径一字不改**。
//
// ⚠️ 为什么要把「非数字」变成 400（core#102 的同族缺陷）：
//
//	旧行为：page=abc ⇒ strconv.Atoi 失败被**忽略** ⇒ 0 ⇒ 归一化成默认页
//	⇒ 调用方以为"我翻到了第 abc 页"，实际拿到的是**第 1 页**，
//	  且服务端**没有任何信号**（无 400、无日志）。
//
// ⚠️ 为什么**不**把「零 / 负数 / 超上限」也变成 400：
//
//	那些是**归一化**的输入，不是解析错误 —— 各域口径（默认页长 / 上限 /
//	超限语义）互不相同，且是对消费方可见的既有契约（响应条数）。
//	把它们变成 400 会让历史调用的响应条数漂移 ⇒ 本层只收紧"非数字"这一类。
//
// ⚠️ 为什么不用 oekit/pkg 的现成原语来解析：
//
//	oekit/pkg/pagination v0.41.0 有 Policy（归一化/偏移/上限）与 PageRequest，
//	但**没有**「从 query 严格解析并报错」的能力；pkg/convert 的 Str2Int 只有
//	「失败返回默认值」这一种宽松语义 —— 正是本轮要治的病。
//	⇒ 解析层自己写（薄），**归一化与偏移一律走 oekit 的 Policy**。
//	（不发 oekit 新版：抬 oekit 会触发"地板不能只抬一半"的连锁发版，本轮不碰。）
package paging

import (
	"fmt"
	"strconv"
)

// ParamError 查询参数的非法取值。
//
// ⚠️ 带上**参数名 + 原值 + 期望格式**：只说"参数错误"等于让调用方回去猜
//
//	（实测：page 传哪种形式才合法，文档里没写）。
type ParamError struct {
	Param  string
	Value  string
	Expect string
}

// Error 实现 error。
func (e *ParamError) Error() string {
	return fmt.Sprintf("参数 %s 取值 %q 非法：期望 %s", e.Param, e.Value, e.Expect)
}

// ParsePageParams 解析分页查询参数（page / page_size），**严格**。
//
// 语义：
//
//   - 取值缺省或空串 ⇒ 返回 0（= "没传"）⇒ 由各域 Policy 归一化。
//   - 取值非空但不是合法整数 ⇒ 返回 *ParamError（点名参数 + 期望格式）
//     ⇒ 调用方统一回 400 + 业务码 10001（参数错误）。
//   - 合法整数（**含 0 / 负数**）⇒ 原样返回，**不归一化**（口径归 Policy 管）。
//
// ⚠️ 返回 0 而不是"默认页 1 / 默认页长 20"是刻意的：
//
//	各域默认页长不同（dao 20 / admin 系统日志 50 / MCP 20 …），
//	解析层若自己填默认值，就得把口径抄一遍 ⇒ 口径表会再次失控
//	（policy.go 的包注释里记着这段方言当初怎么失控的）。
func ParsePageParams(get func(string) string) (page, pageSize int, err error) {
	page, err = parsePageParam(get, "page", "整数（页码；≤0 按该域默认页处理）")
	if err != nil {
		return 0, 0, err
	}
	pageSize, err = parsePageParam(get, "page_size",
		"整数（每页条数；≤0 按该域默认页长处理，超上限按该域口径截断或回落）")
	if err != nil {
		return 0, 0, err
	}
	return page, pageSize, nil
}

// parsePageParam 解析单个分页参数（严格）。
//
// ⚠️ 空串按"没传"处理（不报错）：URL 里的 "page="（空值）语义同"不传"，
//
//	报错会把"没传"误判成"传错了"，徒增噪音。
func parsePageParam(get func(string) string, name, expect string) (int, error) {
	s := get(name)
	if s == "" {
		return 0, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, &ParamError{Param: name, Value: s, Expect: expect}
	}
	return v, nil
}
