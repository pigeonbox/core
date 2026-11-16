package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

// 状态分布整表覆盖：缺席状态必须清零（防上一轮残留误导告警）。
func TestSetBlobStates_OverridesAllStates(t *testing.T) {
	SetBlobStates(map[string]int64{"active": 5, "trashed": 2})
	assert.Equal(t, 5.0, testutil.ToFloat64(blobStates.WithLabelValues("active")))
	assert.Equal(t, 2.0, testutil.ToFloat64(blobStates.WithLabelValues("trashed")))
	assert.Equal(t, 0.0, testutil.ToFloat64(blobStates.WithLabelValues("purged")),
		"缺席状态必须清零（整表覆盖语义）")

	SetBlobStates(map[string]int64{"active": 1})
	assert.Equal(t, 1.0, testutil.ToFloat64(blobStates.WithLabelValues("active")))
	assert.Equal(t, 0.0, testutil.ToFloat64(blobStates.WithLabelValues("trashed")),
		"新一轮必须覆盖旧值")
}

// 漂移 gauge 整表覆盖：缺席 kind 必须清零（防上一轮残留误导告警）。
func TestSetBlobReconcileDrift_OverridesAllKinds(t *testing.T) {
	SetBlobReconcileDrift(map[string]int{"dangling_ledger": 5, "missing_ledger": 2})
	assert.Equal(t, 5.0, BlobReconcileDriftValue("dangling_ledger"))
	assert.Equal(t, 2.0, BlobReconcileDriftValue("missing_ledger"))
	assert.Equal(t, 0.0, BlobReconcileDriftValue("active_object_missing"),
		"缺席 kind 必须清零（整表覆盖语义）")

	SetBlobReconcileDrift(map[string]int{"purged_object_present": 1})
	assert.Equal(t, 0.0, BlobReconcileDriftValue("dangling_ledger"),
		"新一轮必须覆盖旧值")
	assert.Equal(t, 1.0, BlobReconcileDriftValue("purged_object_present"))
}

func TestSetReclaimBacklog(t *testing.T) {
	SetReclaimBacklog(7)
	assert.Equal(t, 7.0, testutil.ToFloat64(blobReclaimBacklog))
	SetReclaimBacklog(0)
	assert.Equal(t, 0.0, testutil.ToFloat64(blobReclaimBacklog))
}
