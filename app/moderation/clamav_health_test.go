package moderation

// ClamAV 存活/降级可观测的断言（core#121）。
//
// 守卫点：
//  1. Ping() 必须**真实探活**并记录结果（ok/fail + 耗时 + 错误摘要）——
//     「Ping 恒绿」的假实现会让本文件的失败侧断言转红（负对照锚点）；
//  2. 近 N 次扫描窗口按结局计数（clean/hit/fail_open/skipped），
//     「degraded 计数」= 窗口内 fail-open 放行次数；
//  3. 窗口有界（≤ scanWindowCap），不会无限增长。
//
// 负对照（已实跑）：把 Ping 改成恒绿（不探活、恒记 ok）⇒ 用例一的失败侧
// 转红；把 recordOutcome(outcomeFailOpen) 去掉 ⇒ 用例二转红。

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClamAV_Health_PingRecordsResult Ping 的结果必须进健康快照。
func TestClamAV_Health_PingRecordsResult(t *testing.T) {
	// 侧一：探通 ⇒ ok
	addr := fakeClamd(t)
	m := NewClamAVModerator(addr, 5, 0, nil)
	require.NoError(t, m.Ping())
	h := m.Health()
	require.NotNil(t, h.LastPing, "Ping 后必须有 last_ping")
	assert.True(t, h.LastPing.OK, "探通时 last_ping.ok 必须为 true")
	assert.Empty(t, h.LastPing.Error)
	assert.False(t, h.LastPing.At.IsZero(), "必须记录探测时刻")

	// 侧二：连不上 ⇒ fail + 错误摘要（负对照锚点：Ping 恒绿实现会让这里转红）
	dead := NewClamAVModerator("127.0.0.1:1", 1, 0, nil)
	assert.Error(t, dead.Ping())
	h = dead.Health()
	require.NotNil(t, h.LastPing)
	assert.False(t, h.LastPing.OK, "探活失败必须如实记录（不得恒绿）")
	assert.NotEmpty(t, h.LastPing.Error, "失败必须带错误摘要")
}

// TestClamAV_Health_FailOpenDegradedCount 近 N 次扫描里 fail-open 放行次数。
func TestClamAV_Health_FailOpenDegradedCount(t *testing.T) {
	dead := NewClamAVModerator("127.0.0.1:1", 1, 0, &fakeOpener{content: "x"})
	for i := 0; i < 3; i++ {
		assert.Equal(t, VerdictAllow, dead.InspectFile(context.Background(), UploadMeta{
			StoragePath: "uploads/x.txt", FileName: "x.txt", Size: 1,
		}), "fail-open 语义保持：扫描器故障必须放行")
	}
	h := dead.Health()
	assert.Equal(t, 3, h.ScansInWindow)
	assert.Equal(t, 3, h.FailOpenInWindow, "degraded 计数 = 窗口内 fail-open 放行次数")
	assert.Equal(t, scanWindowCap, h.WindowSize)

	// 恢复后新扫描进窗口，fail-open 计数随窗口滑动回落（不是只增不减）
	addr := fakeClamd(t)
	ok := NewClamAVModerator(addr, 5, 1024, &fakeOpener{content: "clean"})
	for i := 0; i < 2; i++ {
		assert.Equal(t, VerdictAllow, ok.InspectFile(context.Background(), UploadMeta{
			StoragePath: "uploads/c.txt", FileName: "c.txt", Size: 5,
		}))
	}
	h = ok.Health()
	assert.Equal(t, 2, h.CleanInWindow)
	assert.Equal(t, 0, h.FailOpenInWindow)
}

// TestClamAV_Health_OutcomeKinds 四类扫描结局各自计数。
func TestClamAV_Health_OutcomeKinds(t *testing.T) {
	addr := fakeClamd(t)
	opener := &fakeOpener{content: "clean content"}
	m := NewClamAVModerator(addr, 5, 1024, opener)

	// clean
	assert.Equal(t, VerdictAllow, m.InspectFile(context.Background(), UploadMeta{
		StoragePath: "uploads/a.txt", FileName: "a.txt", Size: 12}))
	// hit
	opener.content = "EICAR payload"
	assert.Equal(t, VerdictReject, m.InspectFile(context.Background(), UploadMeta{
		StoragePath: "uploads/v.txt", FileName: "v.txt", Size: 13}))
	// skipped（超大小上限）
	assert.Equal(t, VerdictAllow, m.InspectFile(context.Background(), UploadMeta{
		StoragePath: "uploads/big.txt", FileName: "big.txt", Size: 2048}))
	// skipped（无路径）
	assert.Equal(t, VerdictAllow, m.InspectFile(context.Background(), UploadMeta{FileName: "t"}))

	h := m.Health()
	assert.Equal(t, 4, h.ScansInWindow)
	assert.Equal(t, 1, h.CleanInWindow)
	assert.Equal(t, 1, h.HitsInWindow)
	assert.Equal(t, 2, h.SkippedInWindow)
	assert.Equal(t, 0, h.FailOpenInWindow)
}

// TestClamAV_Health_WindowBounded 窗口有界：只留最近 scanWindowCap 次。
func TestClamAV_Health_WindowBounded(t *testing.T) {
	dead := NewClamAVModerator("127.0.0.1:1", 1, 0, &fakeOpener{content: "x"})
	for i := 0; i < scanWindowCap+50; i++ {
		dead.InspectFile(context.Background(), UploadMeta{
			StoragePath: "uploads/x.txt", FileName: "x.txt", Size: 1})
	}
	h := dead.Health()
	assert.Equal(t, scanWindowCap, h.ScansInWindow, "窗口必须有界（近 N 次）")
	assert.Equal(t, scanWindowCap, h.FailOpenInWindow)
}

// TestClamAV_TruncateSummary 错误摘要截断（防诊断输出被错误链撑爆）。
func TestClamAV_TruncateSummary(t *testing.T) {
	assert.Equal(t, "short", truncateSummary("  short  ", 100))
	long := strings.Repeat("A", 500)
	got := truncateSummary(long, 200)
	assert.True(t, strings.HasPrefix(got, strings.Repeat("A", 200)), "摘要保留前 200 字符")
	assert.True(t, strings.HasSuffix(got, "…"), "截断要留痕（省略号）")
	assert.Less(t, len(got), 210, "摘要必须真的被截断")
}

// errOpener 恒失败的 ContentOpener（触发 open-failed fail-open 路径）。
type errOpener struct{}

func (errOpener) GetFileReader(context.Context, string) (io.ReadCloser, int64, error) {
	return nil, 0, assert.AnError
}
