package metrics

// upload_state.go —— 上传会话账本状态流转仪表（core#172 第三片·拆分项 3/4）。
//
// pb_blob_upload_transitions_total{from,to,outcome}：分片/presign 会话的
// Pending→Uploading→Committed 段每一次账本推进（含建行）记一笔。
// outcome 取值（与 repo/db/dao 上传段推进结果常量同源）：
//
//	created  —— 首次落行（from=none）
//	advanced —— 沿合法链前进（含逐跳 CAS 走完多步）
//	noop     —— 已在目标态 / 行不存在（无需推进）
//	rejected —— 非法推进（回退/终态复活/未知态）被状态机拒绝
//	error    —— 数据库层错误
//
// 观察口径：
//   - rejected 持续增长 = 有调用方在尝试非法转移（状态机在拦，但值得看）；
//   - error 持续增长 = 账本写失败（best-effort 面，分享主链不受阻，但账本在漂）；
//   - created→advanced 的比例接近 1:3 = 会话健康走完全程（每行 1 建 + 3 跳）。
//
// 与 #207 的 pkg/metrics/blob.go 分工：那个管回收面（pb_blob_states /
// pb_blob_reclaim_backlog），本文件管上传段流转——文件与指标名均不相交。

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

var blobUploadTransitions = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "pb_blob_upload_transitions_total",
	Help: "Blob upload-session ledger transitions by from/to/outcome (created/advanced/noop/rejected/error).",
}, []string{"from", "to", "outcome"})

func init() {
	prometheus.MustRegister(blobUploadTransitions)
}

// RecordBlobUploadTransition 记一笔上传段账本推进（app 层 best-effort 包装内调用；
// outcome 语义见文件头，非法值原样落标签——由调用方约定保证）。
func RecordBlobUploadTransition(from, to, outcome string) {
	blobUploadTransitions.WithLabelValues(from, to, outcome).Inc()
}

// BlobUploadTransitionsValue 读取当前计数（测试/诊断快照用）。
func BlobUploadTransitionsValue(from, to, outcome string) float64 {
	return testutil.ToFloat64(blobUploadTransitions.WithLabelValues(from, to, outcome))
}
