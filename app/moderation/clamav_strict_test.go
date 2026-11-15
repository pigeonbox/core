package moderation

// ClamAV strict 模式断言（core#161）。
//
// 守卫点：
//  1. strict=true + 扫描不可用（clamd 不可达 / 内容源打不开 / 超 max_scan_bytes）
//     ⇒ 拒收（VerdictScanUnavailable），且 degraded 窗口照记（= 拒收计数）；
//  2. 默认 strict=false ⇒ fail-open 放行语义一字不改（负对照锚点）；
//  3. strict 不改「命中拒收 / 干净放行 / 无内容放行」三条既有语义。

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
)

// failingOpener 恒失败的 ContentOpener（触发 open-failed 路径）。
type failingOpener struct{}

func (failingOpener) GetFileReader(context.Context, string) (io.ReadCloser, int64, error) {
	return nil, 0, assert.AnError
}

// TestClamAVStrict_UnreachableRejects strict + clamd 不可达 ⇒ 拒收；默认 ⇒ 放行。
func TestClamAVStrict_UnreachableRejects(t *testing.T) {
	opener := &fakeOpener{content: "x"}
	meta := UploadMeta{StoragePath: "uploads/x.txt", FileName: "x.txt", Size: 1}

	strict := NewClamAVModerator("127.0.0.1:1", 1, 0, opener, true)
	assert.Equal(t, VerdictScanUnavailable, strict.InspectFile(context.Background(), meta),
		"strict：clamd 不可达必须拒收（不得静默放行）")
	h := strict.Health()
	assert.Equal(t, 1, h.FailOpenInWindow,
		"strict 拒收必须计入 degraded 窗口（issue：degraded 计数映射为拒收计数）")
	assert.Equal(t, "scan_unavailable", VerdictScanUnavailable.String())

	// 负对照：默认（strict=false）保持 fail-open 放行
	open := NewClamAVModerator("127.0.0.1:1", 1, 0, opener, false)
	assert.Equal(t, VerdictAllow, open.InspectFile(context.Background(), meta),
		"默认必须保持 fail-open 放行（core#161 破坏性=无）")
}

// TestClamAVStrict_OverSizeRejects strict + 超 max_scan_bytes ⇒ 拒收；默认 ⇒ 跳过放行。
func TestClamAVStrict_OverSizeRejects(t *testing.T) {
	opener := &fakeOpener{content: "x"}
	big := UploadMeta{StoragePath: "uploads/big.bin", FileName: "big.bin", Size: 2048}

	strict := NewClamAVModerator("127.0.0.1:1", 5, 1024, opener, true)
	assert.Equal(t, VerdictScanUnavailable, strict.InspectFile(context.Background(), big),
		"strict：无法完成扫描（超限跳过）必须拒收")

	open := NewClamAVModerator("127.0.0.1:1", 5, 1024, opener, false)
	assert.Equal(t, VerdictAllow, open.InspectFile(context.Background(), big),
		"默认超限跳过语义保持")
}

// TestClamAVStrict_OpenFailedRejects strict + 内容源打不开 ⇒ 拒收。
func TestClamAVStrict_OpenFailedRejects(t *testing.T) {
	m := NewClamAVModerator("127.0.0.1:1", 1, 0, failingOpener{}, true)
	assert.Equal(t, VerdictScanUnavailable, m.InspectFile(context.Background(), UploadMeta{
		StoragePath: "uploads/x.txt", FileName: "x.txt", Size: 1,
	}))
}

// TestClamAVStrict_HitAndCleanUnchanged strict 不改「命中拒收 / 干净放行」语义。
func TestClamAVStrict_HitAndCleanUnchanged(t *testing.T) {
	addr := fakeClamd(t)
	opener := &fakeOpener{content: "clean content"}
	m := NewClamAVModerator(addr, 5, 1024, opener, true)

	assert.Equal(t, VerdictAllow, m.InspectFile(context.Background(), UploadMeta{
		StoragePath: "uploads/ok.txt", FileName: "ok.txt", Size: 13,
	}), "strict 下扫描完成且干净仍放行")

	opener.content = "EICAR payload"
	assert.Equal(t, VerdictReject, m.InspectFile(context.Background(), UploadMeta{
		StoragePath: "uploads/virus.txt", FileName: "virus.txt", Size: 13,
	}), "strict 下命中签名仍走 VerdictReject（与不可用拒收分开）")
}

// TestClamAVStrict_NoContentStillAllows 无内容可扫（文本分享/无路径）⇒ 放行（strict 亦然）。
func TestClamAVStrict_NoContentStillAllows(t *testing.T) {
	m := NewClamAVModerator("127.0.0.1:1", 1, 0, nil, true)
	assert.Equal(t, VerdictAllow, m.InspectFile(context.Background(), UploadMeta{FileName: "text-share"}),
		"文本分享没有 StoragePath：strict 不得误伤（扫描不可用只针对有内容的文件）")
}
