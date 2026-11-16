// Package metrics 文件生命周期账本指标（core#172 第二片·观测）。
//
//   - pb_blob_states{state}        blobs 表状态分布（active/trashed/purge/…）
//   - pb_blob_reclaim_backlog      回收流水线积压（trashed 含宽限期内 +
//     purge 进行中——ReclaimBlobs 每轮**回收后**刷新；持续增长 = 回收跟不上
//     或删对象反复失败，对账面告警依据）
//   - pb_blob_reconcile_drift{kind} 三方对账漂移（账本↔路径↔存储，core#172
//     尾项——ScanBlobDrift 每轮**整表覆盖**刷新；>0 = 有漂移待人工处置，
//     本面只报告不修）
//
// 接线点：admin.ReclaimBlobs（janitor 维护循环，24h 一轮）。未初始化时
// （单测）no-op，零开销。
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

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

// blobReconcileDrift 三方对账漂移（label kind 取值与
// app/admin/blob_reconcile.go 的 Drift* 常量同源）。
var blobReconcileDrift = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "pb_blob_reconcile_drift",
	Help: "Blob ledger/path/storage reconcile drift by kind after last scan (report-only).",
}, []string{"kind"})

func init() {
	prometheus.MustRegister(blobStates, blobReclaimBacklog, blobReconcileDrift)
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

// SetBlobReconcileDrift 刷新四类漂移计数（整表覆盖语义，同 SetBlobStates：
// 传入全量计数，缺席 kind 清零——上一轮的残留不得误导告警）。
// kind 清单与 app/admin/blob_reconcile.go 的 Drift* 常量同源，两侧字符串
// 必须一致（admin 侧扫描测试会断言 gauge 标签值，漂移即红）。
func SetBlobReconcileDrift(counts map[string]int) {
	for _, k := range []string{
		"dangling_ledger", "missing_ledger",
		"active_object_missing", "purged_object_present",
	} {
		blobReconcileDrift.WithLabelValues(k).Set(float64(counts[k]))
	}
}

// BlobReconcileDriftValue 读取某类漂移的当前计数（admin 扫描测试断言
// gauge 标签值与 Drift* 常量同源用；kind 取值见 app/admin/blob_reconcile.go）。
func BlobReconcileDriftValue(kind string) float64 {
	return testutil.ToFloat64(blobReconcileDrift.WithLabelValues(kind))
}
