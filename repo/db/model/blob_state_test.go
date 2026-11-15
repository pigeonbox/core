package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCanBlobTransition_Legal —— issue #172 给定链上的全部合法转移。
func TestCanBlobTransition_Legal(t *testing.T) {
	cases := []struct {
		from, to string
		why      string
	}{
		{BlobPending, BlobUploading, "开始写入"},
		{BlobUploading, BlobCommitted, "字节写完"},
		{BlobCommitted, BlobActive, "首个引用建立"},
		{BlobActive, BlobTrashed, "引用归零进宽限期"},
		{BlobTrashed, BlobActive, "宽限期内被重新引用，恢复"},
		{BlobTrashed, BlobPurge, "宽限期到期，进入回收"},
		{BlobPurge, BlobPurged, "对象删除成功"},
		{BlobPurge, BlobAborted, "回收失败中止（issue: Purge→Purged/Aborted）"},
		{BlobPending, BlobAborted, "上传前中止"},
		{BlobUploading, BlobAborted, "上传中断"},
		{BlobCommitted, BlobAborted, "建分享前放弃"},
	}
	for _, c := range cases {
		assert.Truef(t, CanBlobTransition(c.from, c.to),
			"%s → %s 应合法（%s）", c.from, c.to, c.why)
	}
}

// TestCanBlobTransition_Illegal —— 非法转移一律拒绝：
// 跳态、逆向、终态复活、未知状态、自转移。
func TestCanBlobTransition_Illegal(t *testing.T) {
	cases := []struct {
		from, to string
		why      string
	}{
		// 跳态：绕过中间态
		{BlobPending, BlobActive, "未写入就 Active"},
		{BlobPending, BlobTrashed, "未写入就 Trashed"},
		{BlobUploading, BlobActive, "写入中就 Active"},
		{BlobActive, BlobPurge, "跳过 Trashed 直接回收"},
		{BlobActive, BlobPurged, "跳过两态直接 Purged"},
		{BlobTrashed, BlobPurged, "跳过 Purge 直接 Purged"},
		// 逆向：已提交链不许倒退
		{BlobActive, BlobCommitted, "Active 退回 Committed"},
		{BlobActive, BlobUploading, "Active 退回 Uploading"},
		{BlobCommitted, BlobUploading, "Committed 退回 Uploading"},
		{BlobUploading, BlobPending, "Uploading 退回 Pending"},
		// 终态：已删/已判废不许复活
		{BlobPurged, BlobActive, "Purged 复活"},
		{BlobPurged, BlobTrashed, "Purged 退回 Trashed"},
		{BlobAborted, BlobActive, "Aborted 复活"},
		{BlobAborted, BlobPending, "Aborted 退回 Pending"},
		// 宽限期单向：Trashed 只能恢复或进回收
		{BlobTrashed, BlobCommitted, "Trashed 退回 Committed"},
		// 自转移：无意义，拒绝（调用方不应发起）
		{BlobActive, BlobActive, "自转移"},
		{BlobPurged, BlobPurged, "终态自转移"},
		// 未知状态
		{"", BlobActive, "空状态"},
		{BlobActive, "wat", "未知目标状态"},
		{"wat", BlobActive, "未知源状态"},
	}
	for _, c := range cases {
		assert.Falsef(t, CanBlobTransition(c.from, c.to),
			"%q → %q 应非法（%s）", c.from, c.to, c.why)
	}
}

// TestCanBlobTransition_Reachability —— 全部状态都可达自 Pending（转移表无孤儿态），
// 且终态无出边（Purged/Aborted 不可复活）。
func TestCanBlobTransition_Reachability(t *testing.T) {
	// 从 Pending BFS
	seen := map[string]bool{BlobPending: true}
	queue := []string{BlobPending}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, st := range BlobValidStates() {
			if !seen[st] && CanBlobTransition(cur, st) {
				seen[st] = true
				queue = append(queue, st)
			}
		}
	}
	for _, st := range BlobValidStates() {
		assert.Truef(t, seen[st], "状态 %s 从 Pending 不可达 ⇒ 转移表有孤儿态", st)
	}

	// 终态出边为零
	for _, terminal := range []string{BlobPurged, BlobAborted} {
		for _, st := range BlobValidStates() {
			assert.Falsef(t, CanBlobTransition(terminal, st),
				"终态 %s 不许有任何出边（→ %s）", terminal, st)
		}
	}
}

// TestCanRefTransition —— 引用只释放不复活（硬删不可逆）。
func TestCanRefTransition(t *testing.T) {
	assert.True(t, CanRefTransition(RefActive, RefReleased), "存活引用可释放")
	assert.False(t, CanRefTransition(RefReleased, RefActive), "已释放不许复活（硬删不可逆）")
	assert.False(t, CanRefTransition(RefActive, RefActive), "自转移拒绝")
	assert.False(t, CanRefTransition("", RefActive), "空状态拒绝")
	assert.False(t, CanRefTransition(RefReleased, "wat"), "未知目标拒绝")
}

// TestValidBlobState —— 状态白名单（写入口校验口径）。
func TestValidBlobState(t *testing.T) {
	for _, st := range BlobValidStates() {
		assert.Truef(t, ValidBlobState(st), "%s 应在白名单", st)
	}
	assert.False(t, ValidBlobState("wat"), "未知状态不在白名单")
	assert.False(t, ValidBlobState(""), "空串不在白名单")
}
