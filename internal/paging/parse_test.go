// 分页参数的**严格解析**（core#104：分页解析失败必须显式失败）。
//
// ⚠️ 为什么分页也要严格（与 core#102 同一族缺陷）：
//
//	旧行为：page=abc ⇒ strconv.Atoi 失败被**忽略** ⇒ 0 ⇒ 归一化成默认页
//	⇒ 调用方以为"我翻到了第 abc 页"，实际拿到的是**第 1 页**。
//	core#102 已把「解析失败 ⇒ 静默丢条件 ⇒ 返回全量」定性为缺陷
//	（多给数据比筛不到危险）；分页是同一族问题的轻量版。
//
// ⚠️ 但**只收紧"非数字"这一类**，归一化口径一个字不改：
//
//	page=0 / page=负数 / page_size 超上限 ⇒ 仍按各域 Policy 归一化
//	（那是既有契约：响应条数对消费方可见 ⇒ 改了会让历史调用的条数漂移）。
//
// 负对照：把 parsePageParam 的错误分支去掉（照旧"解析失败就当没传"）
// ⇒ 本文件的 NonNumericRejected 用例红。
package paging

import (
	"strings"
	"testing"
)

// getFromMap 造一个 c.Query 同形的取值函数。
func getFromMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// TestParsePageParams_MissingMeansDefault 缺省（未传 / 空串）⇒ 0/0 且不报错。
//
// 为什么空串不算错：URL 里的 "page="（空值）语义同"没传"
// ⇒ 报错会把"没传"误判成"传错了"，徒增噪音。
func TestParsePageParams_MissingMeansDefault(t *testing.T) {
	page, pageSize, err := ParsePageParams(getFromMap(map[string]string{}))
	if err != nil {
		t.Fatalf("未传分页参数不应报错：%v", err)
	}
	if page != 0 || pageSize != 0 {
		t.Fatalf("未传分页参数应是 (0,0)（交由各域 Policy 归一化），实际 (%d,%d)", page, pageSize)
	}

	page, pageSize, err = ParsePageParams(getFromMap(map[string]string{"page": "", "page_size": ""}))
	if err != nil {
		t.Fatalf("空串分页参数不应报错：%v", err)
	}
	if page != 0 || pageSize != 0 {
		t.Fatalf("空串分页参数应是 (0,0)，实际 (%d,%d)", page, pageSize)
	}
}

// TestParsePageParams_ValidValuesPassThrough 合法值**原样透传**（不归一化）。
//
// 归一化归各域 Policy（口径声明层）⇒ 解析层不得越权改口径。
func TestParsePageParams_ValidValuesPassThrough(t *testing.T) {
	cases := []struct {
		name               string
		page, pageSize     string
		wantPage, wantSize int
	}{
		{"常规", "1", "20", 1, 20},
		{"零（交由 Policy 归一化）", "0", "0", 0, 0},
		{"负数（交由 Policy 归一化）", "-3", "-5", -3, -5},
		{"最大 int64 页码", "9223372036854775807", "20", 9223372036854775807, 20},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			page, pageSize, err := ParsePageParams(getFromMap(map[string]string{"page": c.page, "page_size": c.pageSize}))
			if err != nil {
				t.Fatalf("合法分页参数不应报错：%v", err)
			}
			if page != c.wantPage || pageSize != c.wantSize {
				t.Fatalf("ParsePageParams = (%d,%d)，期望 (%d,%d)", page, pageSize, c.wantPage, c.wantSize)
			}
		})
	}
}

// TestParsePageParams_NonNumericRejected 非数字 ⇒ 显式失败（点名参数 + 期望格式）。
func TestParsePageParams_NonNumericRejected(t *testing.T) {
	cases := []struct {
		name      string
		query     map[string]string
		wantParam string
	}{
		{"page 非数字", map[string]string{"page": "abc"}, "page"},
		{"page_size 非数字", map[string]string{"page_size": "xyz"}, "page_size"},
		{"page 小数", map[string]string{"page": "1.5"}, "page"},
		{"page 科学计数", map[string]string{"page": "1e3"}, "page"},
		{"page 十六进制", map[string]string{"page": "0x10"}, "page"},
		{"page 前导空格", map[string]string{"page": " 5"}, "page"},
		{"page 超 int64", map[string]string{"page": "99999999999999999999"}, "page"},
		{"page_size 超 int64", map[string]string{"page_size": "99999999999999999999"}, "page_size"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := ParsePageParams(getFromMap(c.query))
			if err == nil {
				t.Fatalf("query %v 期望被拒（解析失败必须显式失败），实际 nil", c.query)
			}
			msg := err.Error()
			if !strings.Contains(msg, c.wantParam) {
				t.Fatalf("错误消息必须点名参数 %q，实际 %q", c.wantParam, msg)
			}
			if !strings.Contains(msg, "期望") {
				t.Fatalf("错误消息必须说明期望格式，实际 %q", msg)
			}
		})
	}
}
