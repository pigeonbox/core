// 音频波形缩略图（批 1，2026-11-03）：此前 audio.GenerateThumbnail 是 TODO
// 恒回空，前端只能拿默认音频图标。现用 ffmpeg showwavespic 生成波形 PNG，
// 且 Generate 内嵌消费（失败容忍回空，与 video 截帧同款）。
//
// 断言必须能变红：
//   - 撤 Generate 里的 GenerateThumbnail 消费 ⇒ TestAudioWaveform_WiredIn 红；
//   - 损坏音频时必须**回空不报错**（整体预览不因波形失败判不可用）⇒
//     TestAudioWaveform_DegradeGracefully 红（把 error 上抛即红）。
package preview

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// makeTestWav 用 ffmpeg 合成 1 秒静音 wav（测试素材自产，无仓库二进制）。
func makeTestWav(t *testing.T, dir string) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 不在 PATH，跳过波形真生成用例")
	}
	p := filepath.Join(dir, "tone.wav")
	cmd := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "anullsrc=r=8000", "-t", "1", p)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg 合成素材失败（环境不支持 lavfi）: %v (%s)", err, out)
	}
	return p
}

// TestAudioWaveform_WiredIn 波形已接进 Generate：真生成出 PNG 落盘且
// PreviewData.Thumbnail 指向它。
func TestAudioWaveform_WiredIn(t *testing.T) {
	dir := t.TempDir()
	wav := makeTestWav(t, dir)
	cfg := &Config{
		EnablePreview: true, ThumbnailWidth: 240, ThumbnailHeight: 80,
		PreviewCachePath: filepath.Join(dir, "previews"), FFmpegPath: "ffmpeg",
	}
	g := NewAudioGenerator(cfg)

	data, err := g.Generate(context.Background(), wav, ".wav")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if data.Thumbnail == "" {
		t.Fatal("Thumbnail 为空：波形未接进 Generate（撤内嵌消费即本红）")
	}
	b, err := os.ReadFile(data.Thumbnail)
	if err != nil {
		t.Fatalf("波形文件不可读: %v", err)
	}
	if len(b) < 8 || string(b[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("波形文件不是 PNG（魔数错）: % x", b[:min(8, len(b))])
	}
}

// TestAudioWaveform_ThumbnailErrorReported 损坏音频 ⇒ GenerateThumbnail 如实
// 回 error（容忍在 Generate 的内嵌调用处：err ⇒ thumbnailPath=""，video 同款）。
// 直接断言 error 非空——把「失败吞成空串+nil」即红（调用方无法区分「无波形」
// 与「波形失败」的排查面）。
func TestAudioWaveform_ThumbnailErrorReported(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 不在 PATH")
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.wav")
	if err := os.WriteFile(bad, []byte("not-audio"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		EnablePreview: true, ThumbnailWidth: 240, ThumbnailHeight: 80,
		PreviewCachePath: filepath.Join(dir, "previews"), FFmpegPath: "ffmpeg",
	}
	g := NewAudioGenerator(cfg)

	p, err := g.GenerateThumbnail(context.Background(), bad, 0, 0)
	if err == nil {
		t.Fatalf("损坏音频的波形生成必须报 error（回 %q），失败语义被吞了", p)
	}
}
