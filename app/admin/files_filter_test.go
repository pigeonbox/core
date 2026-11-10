// 管理端文件列表过滤参数的**非法值必须被拒**（core#102）。
//
// ⚠️ 这组用例守护的是一个**语义反转**（2026-11-10 真机实录 103）：
//
//	旧行为：解析失败 ⇒ 条件被静默丢弃 ⇒ 接口返回**全量**
//	（实测 created_after="+0800" ⇒ total=122 = 不带筛选时的全量）
//	⇒ 调用方以为"筛过了只是没筛中"，实际是"**根本没筛**"。
//	⚠️ 「筛选失败」比「筛选为空」危险：**筛选失败是多给数据**。
//
// 负对照：
//
//	① 把 parseAdminFilesFilter 的错误分支去掉（照旧"解析失败就跳过"）⇒ 本用例红；
//	② 把 created_after 的时间解析放宽成"接受 +0800" ⇒ 本用例红（锁住 RFC3339 口径）。
package admin

import (
	"strings"
	"testing"
)

// getFromMap 造一个 c.Query 同形的取值函数。
func getFromMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParseAdminFilesFilter_InvalidValuesRejected(t *testing.T) {
	cases := []struct {
		name      string
		query     map[string]string
		wantParam string // 期望被点名的参数
	}{
		{"时间非 RFC3339", map[string]string{"created_after": "not-a-time"}, "created_after"},
		{"时间无偏移", map[string]string{"created_after": "2026-11-10T09:52:04"}, "created_after"},
		{"时间非法月日", map[string]string{"created_after": "2026-13-45T99:99:99+08:00"}, "created_after"},
		{"时间偏移被空格替换(=URL 里 + 未编码)", map[string]string{"created_after": "2026-11-10T09:52:04 08:00"}, "created_after"},
		{"结束时间同样拒", map[string]string{"created_before": "nope"}, "created_before"},
		{"user_id 非数字", map[string]string{"user_id": "abc"}, "user_id"},
		{"user_id 零", map[string]string{"user_id": "0"}, "user_id"},
		{"user_id 负数", map[string]string{"user_id": "-1"}, "user_id"},
		{"min_size 非数字", map[string]string{"min_size": "abc"}, "min_size"},
		{"min_size 负数", map[string]string{"min_size": "-1"}, "min_size"},
		{"max_size 非数字", map[string]string{"max_size": "1M"}, "max_size"},
		{"max_size 负数", map[string]string{"max_size": "-5"}, "max_size"},
		{"expired 非布尔", map[string]string{"expired": "yes"}, "expired"},
		{"expired 大写", map[string]string{"expired": "TRUE"}, "expired"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseFilesFilter(getFromMap(c.query))
			if err == nil {
				t.Fatalf("query %v 期望被拒（解析失败必须报错，不得静默丢条件），实际 nil", c.query)
			}
			msg := err.Error()
			if !strings.Contains(msg, c.wantParam) {
				t.Fatalf("错误消息必须点名是哪个参数（期望含 %q），实际 %q", c.wantParam, msg)
			}
			if !strings.Contains(msg, "期望") {
				t.Fatalf("错误消息必须说明期望格式，实际 %q", msg)
			}
		})
	}
}

func TestParseAdminFilesFilter_ValidValuesAccepted(t *testing.T) {
	cases := []struct {
		name  string
		query map[string]string
		check func(t *testing.T, q any)
	}{
		{"空查询（不过滤）", map[string]string{}, nil},
		{"带冒号偏移", map[string]string{"created_after": "2026-11-10T09:52:04+08:00"}, nil},
		{"Z 形态", map[string]string{"created_after": "2026-11-10T01:52:04Z"}, nil},
		{"紧凑偏移(date %z 原生形态)", map[string]string{"created_after": "2026-11-10T09:52:04+0800"}, nil},
		{"紧凑负偏移", map[string]string{"created_after": "2026-11-10T09:52:04-0800"}, nil},
		{"小数秒", map[string]string{"created_after": "2026-11-10T09:52:04.123+08:00"}, nil},
		{"合法 user_id", map[string]string{"user_id": "8"}, nil},
		{"合法尺寸", map[string]string{"min_size": "0", "max_size": "1024"}, nil},
		{"合法 expired", map[string]string{"expired": "true"}, nil},
		{"expired=false", map[string]string{"expired": "false"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, err := ParseFilesFilter(getFromMap(c.query))
			if err != nil {
				t.Fatalf("合法输入不应被拒：query %v ⇒ %v", c.query, err)
			}
			_ = q
		})
	}
}

// TestParseAdminFilesFilter_ValidValuesSetConditions 断言合法值**真的落到了条件上**
// （不能只断言"不报错"—— 那会让"解析成功但没赋值"也变绿）。
func TestParseAdminFilesFilter_ValidValuesSetConditions(t *testing.T) {
	q, err := ParseFilesFilter(getFromMap(map[string]string{
		"user_id":       "8",
		"min_size":      "10",
		"max_size":      "1024",
		"created_after": "2026-11-10T09:52:04+08:00",
		"expired":       "true",
	}))
	if err != nil {
		t.Fatalf("合法输入不应被拒：%v", err)
	}
	if q.UserID == nil || *q.UserID != 8 {
		t.Fatalf("user_id 条件未生效：%v", q.UserID)
	}
	if q.MinSize == nil || *q.MinSize != 10 {
		t.Fatalf("min_size 条件未生效：%v", q.MinSize)
	}
	if q.MaxSize == nil || *q.MaxSize != 1024 {
		t.Fatalf("max_size 条件未生效：%v", q.MaxSize)
	}
	if q.CreatedAfter == nil {
		t.Fatal("created_after 条件未生效")
	}
	if q.CreatedAfter.Year() != 2026 || q.CreatedAfter.Minute() != 52 {
		t.Fatalf("created_after 解析结果不对：%v", q.CreatedAfter)
	}
	if q.Expired == nil || !*q.Expired {
		t.Fatalf("expired 条件未生效：%v", q.Expired)
	}
}

// TestParseAdminFilesFilter_PagingNotStrict 锁住**分页不做严格校验**这个决定：
// 分页归一化是既有契约（internal/paging），改成 400 会让历史调用的响应条数漂移；
// 但"非数字"绝不能被当成合法页码 ⇒ 必须归零（= 默认页）。
func TestParseAdminFilesFilter_PagingNotStrict(t *testing.T) {
	q, err := ParseFilesFilter(getFromMap(map[string]string{"page": "abc", "page_size": "xyz"}))
	if err != nil {
		t.Fatalf("分页非法值不应报错（沿用既有归一化口径）：%v", err)
	}
	if q.Page != 0 || q.PageSize != 0 {
		t.Fatalf("非数字分页必须归零（交给 normalizePage 归一化），实际 page=%d page_size=%d", q.Page, q.PageSize)
	}
	neg, err := ParseFilesFilter(getFromMap(map[string]string{"page": "-3", "page_size": "-5"}))
	if err != nil {
		t.Fatalf("负数分页不应报错：%v", err)
	}
	if neg.Page != 0 || neg.PageSize != 0 {
		t.Fatalf("负数分页必须归零，实际 page=%d page_size=%d", neg.Page, neg.PageSize)
	}
}
