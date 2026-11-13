// window.go 公告生效窗口时间的**严格解析**（core#146）。
//
// ⚠️ 这一层与 gen/handler 的分工（与 core#102/#104 的「解析 vs 归一化」同构）：
//
//	解析（本文件）：字符串 → time.Time —— **取值不合法 ⇒ 显式失败**；
//	窗口语义（notify.go 的 Active 过滤）：start_at/end_at 为 NULL = 无边界。
//
// ⚠️ 为什么非法值不能「当没传」（core#146 缺陷实录）：
//
//	旧行为：Create 带 start_at=\"not-a-time\" ⇒ time.Parse 失败被忽略 ⇒
//	落库 NULL ⇒ 公告**立刻全站可见且永不过期**（Active 的条件是
//	\`start_at IS NULL OR start_at <= now\`）——管理员以为「定时生效」，
//	实际是「马上发、永不撤」；Update 同款错误则是**静默 no-op**（200 success）。
//	⇒ 解析失败一律 400 + 10001 + 点名参数，与 #102/#104/#110/#113/#123 同族。
//
// ⚠️ 接受的形态（与 app/admin.parseAdminFilterTime 同口径）：
//
//  1. RFC3339：时间戳 + \"T\" + 带冒号偏移或 \"Z\"（小数秒容忍）；
//
//  2. 紧凑偏移变体 \"+0800\" / \"-0800\"（\`date +%z\` 的原生输出 ⇒ 补冒号后重试）。
//
//     其余形态（空格分隔偏移、\"2026-11-10 09:52:04\"、非法月日时分）一律报错。
package notify

import (
	"fmt"
	"time"
)

// ParseWindowTime 解析公告窗口时间（start_at / end_at）。
//
// 成功 ⇒ (t, nil)；失败 ⇒ (零值, error)，调用方必须回 400（**不得**继续建/改）。
func ParseWindowTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	// 紧凑偏移变体：\"+0800\" / \"-0800\" ⇒ 补成 \"+08:00\" / \"-08:00\" 再试。
	// 只在尾部是 [+-]HHMM 时才改写，其余形态原样失败（⇒ 400）。
	if len(s) > 5 {
		tail := s[len(s)-5:]
		if (tail[0] == '+' || tail[0] == '-') && isAllDigitsWindow(tail[1:]) {
			normalized := s[:len(s)-5] + string(tail[0]) + tail[1:3] + ":" + tail[3:5]
			if t, err := time.Parse(time.RFC3339, normalized); err == nil {
				return t, nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("不是合法的 RFC3339 时间：%q", s)
}

// isAllDigitsWindow 判定字符串是否全为 ASCII 数字（空串返回 false）。
func isAllDigitsWindow(s string) bool {
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
