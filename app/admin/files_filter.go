// files_filter.go 管理端文件列表过滤参数的**解析与校验**（core#102）。
//
// ⚠️ 为什么把它从 handler 里抽成独立函数（本文件）：
//
//	handler 里的解析是**就地解析**（parse 失败即不设条件），既没法单测
//	（必须起 HTTP 才能验），也让"非法值"这件事**没有任何出口**——
//	调用方无从知道"我传的筛选条件根本没生效"。
//	⇒ 抽成纯函数：输入一组 query 取值，输出 (FileCodeQuery, error)，
//	  非法值一律 error ⇒ 调用方（handler）统一 400 + 业务码 10001（参数错误）。
//
// ⚠️ 为什么"解析失败"必须是 400 而不是"当没传"（core#102 真机实录 103）：
//
//	旧行为：created_after 传 "+0800"（紧凑偏移）⇒ time.Parse 失败 ⇒
//	**条件被静默丢弃** ⇒ 接口返回**全量**数据（实测 total=122 = 不带任何
//	筛选时的全量）⇒ 调用方以为"筛过了但没筛中"。
//	⚠️ 「筛选失败」比「筛选为空」危险得多：**筛选失败是"多给数据"**，
//	而"筛选为空"至少诚实地表达了"没有命中"。
//	⇒ 语义改成：非法值 ⇒ 400 + 明确指出是哪个参数、期望什么格式。
//
// 判定口径（逐参数，缺一不可）：
//
//	· 时间（created_after/created_before）：**RFC3339**，偏移带冒号（"+08:00"）
//	  或 "Z"；**紧凑偏移 "+0800" 也接受**（date %z 的原生形态 ⇒ 集成方零改造）。
//	· 数值（min_size/max_size/user_id）：必须是合法整数；
//	  user_id 还必须 > 0（0 不是合法用户 id）；尺寸不得为负。
//	· 布尔（expired）：只接受 "true"/"false"。
//	· 分页（page/page_size）：**严格解析**（core#104）——
//	  非数字 ⇒ 400 + 业务码 10001（点名参数与期望格式）；
//	  零 / 负数 / 超上限 ⇒ 仍按各域 Policy 归一化（**口径不变**）。
package admin

import (
	"fmt"
	"strconv"
	"time"

	"github.com/pigeonbox/core/internal/paging"
	"github.com/pigeonbox/core/repo/db/model"
)

// FilterParamError 管理端过滤参数的非法值（core#102）。
//
// ⚠️ 它现在是 internal/paging.ParamError 的**别名**（不是新类型）：
//
//	分页解析层（internal/paging/parse.go）也需要同一种"点名参数 + 期望格式"的
//	error ⇒ 两处各造一个类型 ⇒ handler 侧要写两套类型断言 ⇒ 迟早漏一处。
//	⇒ 收敛成**一个**类型（core#104）；别名保留旧名 ⇒ 既有引用不用改。
//
// ⚠️ 带上**参数名 + 期望格式**：只说"参数错误"等于让调用方回去猜
//
//	（实测：created_after 传哪种偏移形式才合法，文档里没写）。
type FilterParamError = paging.ParamError

// ParseFilesFilter 解析管理端文件列表的过滤参数（core#102）。
//
// 放在 app/admin（业务层）而不是 gen/handler（生成物目录）：这是一条**业务语义**
// （过滤条件怎么解析），将来 MCP / 其它管理面要复用同一口径时不必再抄一遍。
//
// 与旧行为的**唯一**差别：解析失败不再"当没传"，而是返回错误。
//
//	成功 ⇒ (q, nil)：q 里零值字段 = 该维度不过滤。
//	失败 ⇒ (零值 q, err)：调用方必须回 400，**不得**继续查询。
//
// ⚠️ 分页（page/page_size）：**严格解析**（core#104）——
//
//	非数字 ⇒ 400 + 业务码 10001（点名参数与期望格式）；
//	零 / 负数 / 超上限 ⇒ 仍按各域 Policy 归一化（**口径不变**）。
func ParseFilesFilter(get func(string) string) (model.FileCodeQuery, error) {
	var q model.FileCodeQuery

	q.Keyword = get("keyword")
	q.UploadType = get("upload_type")
	q.OwnerIP = get("owner_ip")
	q.Status = get("status")
	q.Health = get("health")
	q.Deleted = get("deleted")

	if s := get("user_id"); s != "" {
		id, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return q, &FilterParamError{Param: "user_id", Value: s, Expect: "正整数（用户 id）"}
		}
		if id == 0 {
			return q, &FilterParamError{Param: "user_id", Value: s, Expect: "正整数（用户 id，0 无效）"}
		}
		uid := uint(id)
		q.UserID = &uid
	}
	if s := get("min_size"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return q, &FilterParamError{Param: "min_size", Value: s, Expect: "非负整数（字节）"}
		}
		if v < 0 {
			return q, &FilterParamError{Param: "min_size", Value: s, Expect: "非负整数（字节）"}
		}
		q.MinSize = &v
	}
	if s := get("max_size"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return q, &FilterParamError{Param: "max_size", Value: s, Expect: "非负整数（字节）"}
		}
		if v < 0 {
			return q, &FilterParamError{Param: "max_size", Value: s, Expect: "非负整数（字节）"}
		}
		q.MaxSize = &v
	}
	if s := get("created_after"); s != "" {
		t, err := parseAdminFilterTime(s)
		if err != nil {
			return q, &FilterParamError{Param: "created_after", Value: s, Expect: "RFC3339 时间（如 2026-11-10T09:52:04+08:00）；注意 URL 里的 + 要编码成 %2B"}
		}
		q.CreatedAfter = &t
	}
	if s := get("created_before"); s != "" {
		t, err := parseAdminFilterTime(s)
		if err != nil {
			return q, &FilterParamError{Param: "created_before", Value: s, Expect: "RFC3339 时间（如 2026-11-10T09:52:04+08:00）；注意 URL 里的 + 要编码成 %2B"}
		}
		q.CreatedBefore = &t
	}
	if s := get("expired"); s != "" {
		switch s {
		case "true":
			b := true
			q.Expired = &b
		case "false":
			b := false
			q.Expired = &b
		default:
			return q, &FilterParamError{Param: "expired", Value: s, Expect: "true 或 false"}
		}
	}

	// 分页：**严格解析**（core#104）—— 非数字 ⇒ 报错（400 + 10001）。
	//
	// ⚠️ 归一化口径仍**一字不改**：page=0 / 负数 / page_size 超上限 ⇒ 依旧由
	//    dao 侧 normalizePage 按 dao 域口径归一化（既有契约：响应条数对消费方可见）。
	//    本轮只把"非数字"从"静默当默认页"变成显式失败。
	page, pageSize, err := paging.ParsePageParams(get)
	if err != nil {
		return q, err
	}
	q.Page, q.PageSize = page, pageSize
	if q.Page < 0 {
		q.Page = 0
	}
	if q.PageSize < 0 {
		q.PageSize = 0
	}

	return q, nil
}

// parseAdminFilterTime 解析管理端过滤用的时间参数（core#102）。
//
// 接受的形态（**两种都合法**）：
//
//  1. RFC3339：时间戳 + "T" + **带冒号**的偏移或 "Z"
//     "2026-11-10T09:52:04+08:00" / "2026-11-10T01:52:04Z"
//     （Go 的 time.RFC3339 布局 "2006-01-02T15:04:05Z07:00"；小数秒也容忍）
//  2. RFC3339 的**紧凑偏移变体**："2026-11-10T09:52:04+0800"
//     ⇒ Go 原生解析失败（布局要求冒号），这里**补冒号后重试**。
//
// ⚠️ 为什么特意接受紧凑变体：`date +%z` 的**原生输出就是 "+0800"**
//
//	（运维脚本/定时任务最常见的来源）⇒ 让它直接可用，比让每个调用方
//	先知道"要补冒号"再改一遍要省事。
//	实测（103，core v0.39.0 旧行为）："+0800" ⇒ 解析失败 ⇒ 条件被静默丢弃
//	⇒ 返回全量（total=122）；"+08:00" ⇒ 生效（total=0）。
//
// ⚠️ 仍然**接受不了**的形态（⇒ 400，不是静默忽略）：
//
//	· "2026-11-10T09:52:04 08:00"（**空格**分隔的偏移）——这是 query string
//	  里 "+" 被解码成空格的结果（x-www-form-urlencoded 语义）：
//	  直接把 "…+08:00" 拼进 URL 就会变成这样 ⇒ 调用方必须做百分号编码
//	  （curl --data-urlencode / %2B）。本函数修不了 URL，只能说清楚。
//	· "2026-11-10 09:52:04"（空格分隔时间）——非 RFC3339。
//	· "2026-13-45T99:99:99+08:00"（非法月日时分）。
func parseAdminFilterTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	// 紧凑偏移变体："+0800" / "-0800" ⇒ 补成 "+08:00" / "-08:00" 再试。
	// ⚠️ 只在**尾部**是 [+-]HHMM 时才改写，其余形态原样失败（⇒ 400）。
	if len(s) > 5 {
		tail := s[len(s)-5:]
		if (tail[0] == '+' || tail[0] == '-') && isAllDigits(tail[1:]) {
			normalized := s[:len(s)-5] + string(tail[0]) + tail[1:3] + ":" + tail[3:5]
			if t, err := time.Parse(time.RFC3339, normalized); err == nil {
				return t, nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("不是合法的 RFC3339 时间：%q", s)
}

// isAllDigits 判定字符串是否全为 ASCII 数字（空串返回 false）。
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
