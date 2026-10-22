package utils

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/pigeonbox/core/conf"
)

// allExpireStyles 全部合法的过期样式
var allExpireStyles = map[string]bool{
	"minute": true, "hour": true, "day": true, "week": true,
	"month": true, "year": true, "forever": true, "count": true,
}

// IsValidExpireStyle 样式是否合法（在已知样式表内）
func IsValidExpireStyle(style string) bool {
	return allExpireStyles[strings.ToLower(style)]
}

// AllExpireStyles 返回全部合法过期样式（**有序**，便于前端渲染与对账）。
//
// 2026-10-22：这是「过期样式合法集合」的**唯一真相源**。此前
// setup_service.go / app/mcp/tools.go / bootstrap/handlers_public.go
// 各有一份独立白名单，**三份都漏了 "count"**（按取件次数过期）⇒
// 管理员勾了会被静默丢弃、前端根本选不到、MCP 判非法——同一能力四种行为。
// 新增/调整样式只改这一处。
func AllExpireStyles() []string {
	out := make([]string, 0, len(allExpireStyles))
	for _, s := range expireStyleOrder {
		if allExpireStyles[s] {
			out = append(out, s)
		}
	}
	return out
}

// expireStyleOrder 全集的规定顺序（与前端向导选项顺序一致）。
var expireStyleOrder = []string{"minute", "hour", "day", "week", "month", "year", "forever", "count"}

// CheckExpireStyleAllowed 过期样式白名单校验（对标上游"管理员裁剪过期样式"）。
// upload.allowed_expire_styles 为空 = 全部允许；否则样式必须在列表内。
// 空样式 = 未指定（历史语义，下游 CalculateExpireTime 会默认 day），放行。
func CheckExpireStyleAllowed(style string) error {
	style = strings.ToLower(style)
	if style == "" {
		return nil
	}
	if !IsValidExpireStyle(style) {
		return errors.New("无效的过期样式: " + style)
	}
	cfg := conf.GetGlobalConfig()
	if cfg == nil || len(cfg.Upload.AllowedExpireStyles) == 0 {
		return nil
	}
	for _, s := range cfg.Upload.AllowedExpireStyles {
		if strings.ToLower(s) == style {
			return nil
		}
	}
	return errors.New("该过期样式已被管理员禁用: " + style)
}

// ExpireParams 过期参数
type ExpireParams struct {
	ExpireValue int
	ExpireStyle string
	RequireAuth bool
}

// ParseExpireParams 解析过期参数
func ParseExpireParams(expireValueStr, expireStyle, requireAuthStr string) (*ExpireParams, error) {
	expireValue, err := strconv.Atoi(expireValueStr)
	if err != nil {
		return nil, errors.New("过期值必须是数字")
	}

	if expireValue <= 0 && expireStyle != "forever" {
		return nil, errors.New("过期值必须大于0")
	}

	requireAuth := requireAuthStr == "true"

	return &ExpireParams{
		ExpireValue: expireValue,
		ExpireStyle: expireStyle,
		RequireAuth: requireAuth,
	}, nil
}

// GetMaxSaveSecondsCap 全局过期时间上限（秒），upload.max_save_seconds_cap，0 = 不限。
func GetMaxSaveSecondsCap() int64 {
	cfg := conf.GetGlobalConfig()
	if cfg == nil {
		return 0
	}
	return cfg.Upload.MaxSaveSecondsCap
}

// CalculateExpireTime 计算过期时间。
// 全局上限钳制：upload.max_save_seconds_cap > 0 且时长超过上限时钳到上限
// （此前该配置定义了但无任何调用方，形同虚设）。
func CalculateExpireTime(expireValue int, expireStyle string) *time.Time {
	if expireStyle == "forever" {
		return nil
	}

	// 修复：当不传参数时，设置默认值为1天
	// 这样避免了 expireValue=0 和 expireStyle="" 导致立即过期的问题
	if expireValue <= 0 {
		expireValue = 1
	}
	if expireStyle == "" {
		expireStyle = "day"
	}

	var duration time.Duration
	switch expireStyle {
	case "minute":
		duration = time.Duration(expireValue) * time.Minute
	case "hour":
		duration = time.Duration(expireValue) * time.Hour
	case "day":
		duration = time.Duration(expireValue) * 24 * time.Hour
	case "week":
		duration = time.Duration(expireValue) * 7 * 24 * time.Hour
	case "month":
		duration = time.Duration(expireValue) * 30 * 24 * time.Hour
	case "year":
		duration = time.Duration(expireValue) * 365 * 24 * time.Hour
	default:
		duration = time.Duration(expireValue) * 24 * time.Hour
	}

	if cap := GetMaxSaveSecondsCap(); cap > 0 && duration > time.Duration(cap)*time.Second {
		duration = time.Duration(cap) * time.Second
	}

	expireTime := time.Now().Add(duration)
	return &expireTime
}

// CalculateExpireCount 计算过期次数（-1 表示无限制）
func CalculateExpireCount(expireStyle string, expireValue int) int {
	if expireStyle == "count" {
		return expireValue
	}
	return -1 // 无限制次数
}
