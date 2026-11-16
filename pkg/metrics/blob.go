// Package metrics 文件生命周期账本指标（core#172 第二片·观测）。
//
//   - pb_blob_states{state}        blobs 表状态分布（active/trashed/purge/…）
//   - pb_blob_reclaim_backlog      回收流水线积压（trashed 含宽限期内 +
//     purge 进行中——ReclaimBlobs 每轮**回收后**刷新；持续增长 = 回收跟不上
//     或删对象反复失败，对账面告警依据）
//
// 接线点：admin.ReclaimBlobs（janitor 维护循环，24h 一轮）。未初始化时
// （单测）no-op，零开销。
package metrics

import "github.com/prometheus/client_golang/prometheus"

// blobStates 账本状态分布（label = model.Blob* 状态值）。
var blobStates = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "pb_blob_states",
	Help: "Blob ledger rows by lifecycle state.",
}, []string{"state"})

// blobReclaimBacklog 回收流水线积压（trashed + purge 态行数）。
var blobReclaimBacklog = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "pb_blob_reclaim_backlog",
	Help: "Blob rows in the reclaim pipeline (trashed or purge) after last run.",
})

func init() {
	prometheus.MustRegister(blobStates, blobReclaimBacklog)
}

// SetBlobStates 刷新状态分布（整表覆盖语义：传入全量计数；缺席状态清零）。
func SetBlobStates(counts map[string]int64) {
	for _, s := range []string{
		"pending", "uploading", "committed", "active",
		"trashed", "purge", "purged", "aborted",
	} {
		blobStates.WithLabelValues(s).Set(float64(counts[s]))
	}
}

// SetReclaimBacklog 刷新回收积压。
func SetReclaimBacklog(n int) {
	blobReclaimBacklog.Set(float64(n))
}
