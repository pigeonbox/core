package paircode

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestNormalize 归一语义：去空白/连字符、大写、12 位按 4-4-4 分组。
//
// 搬迁前的两份同值副本（app/p2px 与 app/federation 各一份）语义逐字对齐，
// 此处固化为单源断言——任一域改语义都会在此变红。
func TestNormalize(t *testing.T) {
	// 裸串 → 4-4-4 分组
	assert.Equal(t, "KJ8Y-WCYZ-E249", Normalize("KJ8YWCYZE249"))
	// 小写 → 大写
	assert.Equal(t, "KJ8Y-WCYZ-E249", Normalize("kj8ywcyze249"))
	// 已带连字符 → 归一后形态一致
	assert.Equal(t, "KJ8Y-WCYZ-E249", Normalize("KJ8Y-WCYZ-E249"))
	// 首尾空白
	assert.Equal(t, "KJ8Y-WCYZ-E249", Normalize("  KJ8Y-WCYZ-E249\n"))
	// 空串
	assert.Equal(t, "", Normalize(""))
	assert.Equal(t, "", Normalize("   "))
	// 非 12 位不做分组（格式校验交调用方）
	assert.Equal(t, "SHORT", Normalize("short"))
	assert.Equal(t, "ABCDEFGHIJK", Normalize("ABCDEFGHIJK"))
	assert.Equal(t, "ABCDEFGHIJKLM", Normalize("ABCDEFGHIJKLM"))
}

// TestNormalizeIdempotent 归一幂等：归一后再归一形态不变。
func TestNormalizeIdempotent(t *testing.T) {
	once := Normalize("kj8ywcyze249")
	assert.Equal(t, once, Normalize(once))
}
