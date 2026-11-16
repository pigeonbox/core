package metrics

// gc.go —— 对象 GC 任务积压仪表（core#192）。
//
// pb_gc_jobs{state=pending|running|done|skipped|dead}：每轮 worker 末刷新。
// pending 持续走高 = 删除产能跟不上；dead > 0 = 有对象反复删除失败需要
// 人工介入（管理端可重放）。

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

var gcJobs = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "pb_gc_jobs",
	Help: "Object GC jobs by state (pending/running/done/skipped/dead).",
}, []string{"state"})

func init() {
	prometheus.MustRegister(gcJobs)
}

// RecordGCJobs 按状态刷新任务计数仪表（每轮 worker 末调用一次）。
// counts 为空 map 时按零值刷新（上一轮残留计数清零——任务清空也是可观测状态）。
func RecordGCJobs(counts map[string]int64) {
	for _, state := range []string{"pending", "running", "done", "skipped", "dead"} {
		gcJobs.WithLabelValues(state).Set(float64(counts[state]))
	}
}

// GCJobsGaugeValue 读取当前仪表值（测试/诊断快照用）。
func GCJobsGaugeValue(state string) float64 {
	return testutil.ToFloat64(gcJobs.WithLabelValues(state))
}
