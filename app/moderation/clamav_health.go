package moderation

// ClamAV 存活与降级可观测（core#121）。
//
// 背景：ClamAV 接线是 fail-open（连不上/超时 ⇒ 放行）——可用性优先是对的，
// 但此前是**静默失效**：clamd 挂掉时没有任何信号（Ping 零生产调用方），
// 病毒扫描事实上关闭而管理台仍显示「已启用」。本文件只补可观测，
// **不改 fail-open 语义**（业务永远不被扫描器阻塞）。
//
// 两个信号源：
//   - last_ping：Ping() 最近一次结果（ok/fail + 耗时 + 错误摘要）。
//     生产调用方 = GET /admin/diagnostics（读取前惰性探测，见
//     bootstrap/diagnostics.go 的 diagnosticsModeration）。
//   - 近 N 次扫描窗口：每次 InspectFile 的结局（clean/hit/fail_open/skipped）；
//     「degraded 计数」= 窗口内因 fail-open 放行的次数。
//
// 这里没有告警通道——告警由消费方（pb doctor / 管理台 / 巡检脚本）按
// 自己的口径做；本包只负责把事实记准。

import (
	"strings"
	"sync"
	"time"
)

// scanWindowCap 扫描窗口容量（近 N 次；只增不减的棘轮计数会被单次故障
// 永久污染 ⇒ 用窗口计数，恢复后计数随窗口自然回落）。
const scanWindowCap = 100

// pingErrorCap 错误摘要长度上限（诊断输出放摘要，不放整条错误链/响应体）。
const pingErrorCap = 200

// 扫描结局（窗口内的单次 InspectFile 记录）。
const (
	outcomeClean    = "clean"     // 扫描完成且干净
	outcomeHit      = "hit"       // 命中签名（调用方按 VerdictReject 拒绝）
	outcomeFailOpen = "fail_open" // 扫描失败按 fail-open 放行
	outcomeSkipped  = "skipped"   // 未扫描（超大小上限/无路径/无 opener，按策略放行）
)

// ClamAVPingResult 一次探活结果。
type ClamAVPingResult struct {
	// OK 是否探通（clamd 回 PONG）。
	OK bool `json:"ok"`
	// At 探测时刻。
	At time.Time `json:"at"`
	// DurationMs 耗时（毫秒）。
	DurationMs int64 `json:"duration_ms"`
	// Error 失败原因摘要（OK=true 时为空；超长截断到 pingErrorCap）。
	Error string `json:"error,omitempty"`
}

// ClamAVHealth 扫描器健康快照（诊断端点消费）。
type ClamAVHealth struct {
	// LastPing 最近一次探活结果（从未探活时为 nil）。
	LastPing *ClamAVPingResult `json:"last_ping,omitempty"`
	// WindowSize 扫描窗口容量（近 N 次）。
	WindowSize int `json:"window_size"`
	// ScansInWindow 窗口内已记录的扫描次数。
	ScansInWindow int `json:"scans_in_window"`
	// FailOpenInWindow 窗口内因 fail-open 放行的次数（= degraded 计数）。
	FailOpenInWindow int `json:"fail_open_in_window"`
	// HitsInWindow 窗口内命中签名的次数。
	HitsInWindow int `json:"hits_in_window"`
	// CleanInWindow 窗口内扫描干净的次数。
	CleanInWindow int `json:"clean_in_window"`
	// SkippedInWindow 窗口内未扫描（策略跳过）的次数。
	SkippedInWindow int `json:"skipped_in_window"`
}

// clamavState 扫描器运行状态（并发安全；由 ClamAVModerator 持有）。
type clamavState struct {
	mu       sync.Mutex
	lastPing *ClamAVPingResult
	outcomes []string // 环形窗口（满则丢最旧）
}

// recordPing 记录一次探活结果（错误截断成摘要）。
func (s *clamavState) recordPing(d time.Duration, err error) {
	res := &ClamAVPingResult{OK: err == nil, At: time.Now(), DurationMs: d.Milliseconds()}
	if err != nil {
		res.Error = truncateSummary(err.Error(), pingErrorCap)
	}
	s.mu.Lock()
	s.lastPing = res
	s.mu.Unlock()
}

// recordOutcome 记录一次扫描结局进窗口。
func (s *clamavState) recordOutcome(outcome string) {
	s.mu.Lock()
	s.outcomes = append(s.outcomes, outcome)
	if len(s.outcomes) > scanWindowCap {
		s.outcomes = s.outcomes[len(s.outcomes)-scanWindowCap:]
	}
	s.mu.Unlock()
}

// snapshot 生成健康快照。
func (s *clamavState) snapshot() ClamAVHealth {
	h := ClamAVHealth{WindowSize: scanWindowCap}
	s.mu.Lock()
	defer s.mu.Unlock()
	h.LastPing = s.lastPing
	h.ScansInWindow = len(s.outcomes)
	for _, o := range s.outcomes {
		switch o {
		case outcomeClean:
			h.CleanInWindow++
		case outcomeHit:
			h.HitsInWindow++
		case outcomeFailOpen:
			h.FailOpenInWindow++
		case outcomeSkipped:
			h.SkippedInWindow++
		}
	}
	return h
}

// truncateSummary 错误摘要截断（防诊断输出被整条错误链撑爆）。
func truncateSummary(msg string, max int) string {
	msg = strings.TrimSpace(msg)
	if len(msg) <= max {
		return msg
	}
	return msg[:max] + "…"
}
