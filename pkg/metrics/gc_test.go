package metrics

// core#192：pb_gc_jobs 仪表刷新（数据面 CountByState 的呈现层）。
// 独立包单测 ⇒ 全局 gauge 无同包其他用例污染，断言绝对值。

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRecordGCJobs(t *testing.T) {
	RecordGCJobs(map[string]int64{
		"pending": 7,
		"running": 2,
		"done":    100,
		"skipped": 3,
		"dead":    1,
	})
	assert.EqualValues(t, 7, GCJobsGaugeValue("pending"))
	assert.EqualValues(t, 2, GCJobsGaugeValue("running"))
	assert.EqualValues(t, 100, GCJobsGaugeValue("done"))
	assert.EqualValues(t, 3, GCJobsGaugeValue("skipped"))
	assert.EqualValues(t, 1, GCJobsGaugeValue("dead"))

	// 清空也是可观测状态：缺失的状态按零值刷新（残留计数清零）
	RecordGCJobs(map[string]int64{"done": 105})
	assert.EqualValues(t, 0, GCJobsGaugeValue("pending"), "缺失状态必须清零")
	assert.EqualValues(t, 105, GCJobsGaugeValue("done"))
}
