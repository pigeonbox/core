package handler

// 访客投递响应的 data 段（2026-11-01 跨站投递增量）。
//
// 契约：request.thrift 的 GuestUploadData 增 `2: optional string pickup_code`
// ⇒ 对方柜（B 柜）落成分享后把**本站铸的** 6 位码回给发起柜（A 柜）透传。
//
// ⚠️ optional 语义 = **缺字段 ≠ 空串**：没铸出码时不得回一个空串
// （空串会让 A 柜误判成「有码但码为空」，前端也会显示空白码框）。
//
// 负对照：把 guestUploadData 里的 `if pickupCode != ""` 判断去掉（恒填）
// ⇒ 「没码时不得有 pickup_code 键」这条断言必红。

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGuestUploadData_PickupCodeOptional(t *testing.T) {
	// 铸出码 ⇒ 带 pickup_code
	data := guestUploadData(2, "AB12CD")
	assert.Equal(t, 2, data["file_count"])
	assert.Equal(t, "AB12CD", data["pickup_code"])

	// 未铸出码 ⇒ **不带** pickup_code 键（不是空串）
	data = guestUploadData(1, "")
	_, has := data["pickup_code"]
	assert.False(t, has, "未铸出码时不得回空串（optional 语义：缺字段 ≠ 空串）")

	raw, err := json.Marshal(data)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "pickup_code")
}
