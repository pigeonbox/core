package metrics

import (
	"testing"
)

// 计数与标签基本盘：按 (from,to,outcome) 精确累计，读数可复现。
func TestRecordBlobUploadTransition_Counts(t *testing.T) {
	// 用本测试专属标签值，避免与其他测试/并行用例共享序列互相干扰。
	RecordBlobUploadTransition("pending", "uploading", "advanced")
	RecordBlobUploadTransition("pending", "uploading", "advanced")
	RecordBlobUploadTransition("none", "pending", "created")

	if got := BlobUploadTransitionsValue("pending", "uploading", "advanced"); got != 2 {
		t.Fatalf("advanced 计数 = %v, want 2", got)
	}
	if got := BlobUploadTransitionsValue("none", "pending", "created"); got != 1 {
		t.Fatalf("created 计数 = %v, want 1", got)
	}
	// 负对照：不同 outcome 标签互不串账
	if got := BlobUploadTransitionsValue("pending", "uploading", "rejected"); got != 0 {
		t.Fatalf("rejected 计数 = %v, want 0（标签隔离失效）", got)
	}
}
