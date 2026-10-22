package utils

import (
	"testing"
	"time"

	"github.com/pigeonbox/core/conf"
	"github.com/stretchr/testify/assert"
)

func TestCalculateExpireTime_Basic(t *testing.T) {
	now := time.Now()
	tp := CalculateExpireTime(2, "hour")
	assert.NotNil(t, tp)
	diff := tp.Sub(now)
	assert.Greater(t, diff, 1*time.Hour)
	assert.Less(t, diff, 3*time.Hour)
}

func TestCalculateExpireTime_Forever(t *testing.T) {
	assert.Nil(t, CalculateExpireTime(1, "forever"))
}

func TestCalculateExpireTime_CapClamp(t *testing.T) {
	t.Run("超过上限被钳制", func(t *testing.T) {
		old := conf.GetGlobalConfig()
		conf.SetGlobalConfig(&conf.AppConfiguration{Upload: conf.UploadConfig{MaxSaveSecondsCap: 3600}})
		t.Cleanup(func() { conf.SetGlobalConfig(old) })

		start := time.Now()
		tp := CalculateExpireTime(2, "hour") // 请求 2h，上限 1h
		diff := tp.Sub(start)
		assert.GreaterOrEqual(t, diff, 3599*time.Second)
		assert.LessOrEqual(t, diff, 3601*time.Second)
	})
	t.Run("未超上限不受影响", func(t *testing.T) {
		old := conf.GetGlobalConfig()
		conf.SetGlobalConfig(&conf.AppConfiguration{Upload: conf.UploadConfig{MaxSaveSecondsCap: 7200}})
		t.Cleanup(func() { conf.SetGlobalConfig(old) })

		start := time.Now()
		tp := CalculateExpireTime(1, "hour")
		diff := tp.Sub(start)
		assert.Greater(t, diff, 59*time.Minute)
		assert.Less(t, diff, 61*time.Minute)
	})
	t.Run("上限为0不限", func(t *testing.T) {
		withUploadConfig(t, conf.UploadConfig{MaxSaveSecondsCap: 0})
		start := time.Now()
		tp := CalculateExpireTime(1, "year")
		assert.Greater(t, tp.Sub(start), 300*24*time.Hour)
	})
}

func TestCalculateExpireCount(t *testing.T) {
	assert.Equal(t, 5, CalculateExpireCount("count", 5))
	assert.Equal(t, -1, CalculateExpireCount("day", 5))
}

// TestExpireStyleSingleSource 钉住「过期样式合法集合只有一处真相源」。
//
// 2026-10-22 实弹：setup_service.go 与 app/mcp/tools.go 各有一份独立白名单，
// 两份都漏了 "count"（按取件次数过期），而本文件的 allExpireStyles 有
// ⇒ 管理员/向导提交的 count 被**静默丢弃**，MCP 入口也把 count 判非法，
// 而网页分享能用 —— 同一能力三个入口三种行为。
//
// 本测试把「count 必须被所有入口接受」钉成回归：任一入口再写独立白名单
// 而漏项，本用例即红。
func TestExpireStyleSingleSource(t *testing.T) {
	// 全部合法样式（含 count）
	for _, s := range []string{"minute", "hour", "day", "week", "month", "year", "forever", "count"} {
		assert.Truef(t, IsValidExpireStyle(s), "合法样式 %q 被判非法（白名单与 pkg/utils 漂移）", s)
	}
	// 明确非法的（注意：判定会 ToLower，故 "COUNT" 合法）
	for _, s := range []string{"", "second", "days", "weekday"} {
		assert.Falsef(t, IsValidExpireStyle(s), "非法样式 %q 竟被接受", s)
	}
	// count 必须能算出次数（不是仅被接受）
	assert.Equal(t, 3, CalculateExpireCount("count", 3))
}
