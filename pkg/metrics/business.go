// Package metrics 业务指标（治理 2026-10-03）。
//
// 此前 Prometheus 只有 3 个 HTTP RED 指标，"上传了多少、被拒了多少、为什么被拒"
// 完全不可观测。本文件补业务计数器（Counter 天然廉价，无需开关）：
//   - pb_upload_bytes_total{channel}       上传字节累计
//   - pb_share_created_total{upload_type}  分享创建数
//   - pb_upload_rejected_total{reason}     上传/取件拒绝数（按原因）
//   - pb_moderation_hits_total{action}     审核命中数（reject/pending）
//
// 接线点：share service（创建/审核）、pkg/gate（闸门拒绝）、pkg/utils（类型拒绝）。
// 未初始化（单测/未启用 metrics 中间件）时全部 no-op，零开销。
package metrics

import "github.com/prometheus/client_golang/prometheus"

// 拒绝原因取值（reason label）。
const (
	RejectType      = "type"       // 扩展名/魔数拒绝
	RejectSize      = "size"       // 大小超限
	RejectQuota     = "quota"      // 存储配额/单次上限
	RejectRateLimit = "ratelimit"  // QPS 限流
	RejectDisabled  = "disabled"   // open_upload/require_login
	RejectBlocked   = "blocked"    // 分享被禁用/待审取件
	RejectModerated = "moderation" // 审核拒绝（内容命中）
	// RejectScanUnavailable 扫描不可用拒收（core#161：clamav strict 下扫描器
	// 不可达/超时/超扫描上限）——与内容命中分开，便于区分「扫描器挂了」与
	// 「文件真违规」。
	RejectScanUnavailable = "scan_unavailable"
	RejectAnonQuota       = "anon_quota" // 匿名日配额
)

var (
	uploadBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pb_upload_bytes_total",
		Help: "Total uploaded bytes.",
	}, []string{"channel"})

	shareCreated = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pb_share_created_total",
		Help: "Total shares created.",
	}, []string{"upload_type"})

	uploadRejected = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pb_upload_rejected_total",
		Help: "Upload/pickup rejections by reason.",
	}, []string{"reason"})

	moderationHits = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pb_moderation_hits_total",
		Help: "Moderation hook hits by action.",
	}, []string{"action"})
)

func init() {
	prometheus.MustRegister(uploadBytes, shareCreated, uploadRejected, moderationHits)
}

// RecordUploadBytes 累计上传字节（channel: direct/chunk/presign/anonymous）。
func RecordUploadBytes(channel string, bytes int64) {
	uploadBytes.WithLabelValues(channel).Add(float64(bytes))
}

// RecordShareCreated 分享创建计数。
func RecordShareCreated(uploadType string) {
	shareCreated.WithLabelValues(uploadType).Inc()
}

// RecordRejected 拒绝计数（reason 取本文件 Reject* 常量）。
func RecordRejected(reason string) {
	uploadRejected.WithLabelValues(reason).Inc()
}

// RecordModerationHit 审核命中计数（action: reject/pending）。
func RecordModerationHit(action string) {
	moderationHits.WithLabelValues(action).Inc()
}
