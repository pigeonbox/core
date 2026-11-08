// 预览子进程的凭据门禁与输出封顶（core#84，2026-11-09）
//
// 判据与负对照（每条断言怎么变红）：
//   - TestChildEnv_Allowlist：把 childEnv 换成 os.Environ() 直通（allowlist
//     失效）⇒ 凭据哨兵残留即红；删掉 PATH/TMPDIR 的保留逻辑 ⇒ 正对照红
//     （环境被清空会让 ffmpeg 起不来——判据要能同时抓住两个方向的回归）；
//   - TestChildEnv_NegativeControl：不设 cmd.Env 的桩必须能扫到哨兵——
//     证明判据真的在量环境而不是扫了个空壳（判据失真时红，防「全绿但
//     什么都没量到」）；
//   - TestRunProbe_EnvAllowlisted：newProbeCmd 漏设 cmd.Env ⇒ 子进程继承
//     全量环境 ⇒ 哨兵进输出即红（主行为判据，五个调用点全部经它）；
//   - TestProbe_BoundedCapture：tailBuffer 换成无界缓冲 ⇒ 10MB 全量入内存
//     ⇒ 封顶断言红；Write 改成「读满 cap 就停读」⇒ 子进程阻塞在 write、
//     只能等 30s ctx 超时 ⇒ DeadlineExceeded 断言红（排水语义）；
//   - TestTailBuffer_RetainsTail：截头留尾改成截尾留头 ⇒ 断言红；
//   - TestPreviewSpawnsGoThroughProbeCmd：任何调用点绕开 newProbeCmd 直起
//     exec ⇒ 源码守卫红（机械守卫：新增调用点忘了走门禁同样红）。
package preview

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	sentinelAdminPW = "sentinel-admin-password-9f14d2"
	sentinelJWT     = "sentinel-jwt-secret-4c72a1"
	sentinelS3      = "sentinel-s3-secret-8b35c0"
)

// setSentinels 注入三个凭据哨兵（与 core 真实凭据变量同形态：管理口令/
// JWT 签名密钥/存储后端凭据）。
func setSentinels(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"PB_ADMIN_PASSWORD":    sentinelAdminPW,
		"PB_JWT_SECRET":        sentinelJWT,
		"PB_STORAGE_S3_SECRET": sentinelS3,
	} {
		t.Setenv(k, v)
	}
}

// envHas 判断环境切片是否含键 k。
func envHas(env []string, k string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, k+"=") {
			return true
		}
	}
	return false
}

// TestChildEnv_Allowlist 纯单元：凭据哨兵不在 allowlist ⇒ 剔除；
// PATH/TMPDIR 正对照保留（正对照键用 t.Setenv 显式落值，不依赖测试进程
// 恰好带着它们）。
func TestChildEnv_Allowlist(t *testing.T) {
	setSentinels(t)
	t.Setenv("PATH", "/pb-test-path")
	t.Setenv("TMPDIR", "/pb-test-tmpdir")
	env := childEnv()
	envText := strings.Join(env, "\n")
	for _, s := range []string{sentinelAdminPW, sentinelJWT, sentinelS3} {
		if strings.Contains(envText, s) {
			t.Errorf("childEnv() 剔除失败: %s 残留", s)
		}
	}
	if !envHas(env, "PATH") {
		t.Error("PATH 未保留（正对照：环境清空会让 ffmpeg 起不来）")
	}
	if !envHas(env, "TMPDIR") {
		t.Error("TMPDIR 未保留（ffmpeg 写临时文件需要）")
	}
}

// TestChildEnv_NegativeControl 负对照：不设 cmd.Env 时哨兵必须能扫到
// （证明判据真的在量环境，不是扫了个空壳）。
func TestChildEnv_NegativeControl(t *testing.T) {
	setSentinels(t)
	// 负对照桩：故意走默认继承（cmd.Env == nil）
	out, err := exec.Command("sh", "-c", "env").CombinedOutput()
	if err != nil {
		t.Fatalf("负对照 sh 桩跑不起来: %v", err)
	}
	if !strings.Contains(string(out), sentinelAdminPW) {
		t.Fatal("负对照失真：默认继承的环境应能扫到哨兵")
	}
}

// TestRunProbe_EnvAllowlisted 行为主判据：经 runProbeOutput 起的子进程
// 环境里扫不到凭据哨兵（video/audio 五个调用点全部经由它）+ PATH 正对照。
func TestRunProbe_EnvAllowlisted(t *testing.T) {
	setSentinels(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, _, err := runProbeOutput(ctx, "sh", "-c", "env")
	if err != nil {
		t.Fatalf("runProbeOutput: %v", err)
	}
	for _, s := range []string{sentinelAdminPW, sentinelJWT, sentinelS3} {
		if strings.Contains(out, s) {
			t.Errorf("子进程环境扫到哨兵 %s——泄漏面未关闭", s)
		}
	}
	if !strings.Contains(out, "PATH=") {
		t.Error("PATH 正对照缺失（环境被清空会让 ffmpeg 起不来）")
	}
}

// TestProbe_BoundedCapture 有界捕获：10MB stderr（4096B × 2560 块）⇒ 封顶
// 保留尾部、快速完成不阻塞（排水语义），exit 1 如实上抛。
func TestProbe_BoundedCapture(t *testing.T) {
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh 不存在")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	noise := "dd if=/dev/zero bs=4096 count=2560 1>&2; exit 1"
	out, errOut, err := runProbeOutput(ctx, "/bin/sh", "-c", noise)
	if err == nil {
		t.Fatal("exit 1 的桩必须如实回 error")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("排水语义失效：子进程阻塞在 write 上等到 ctx 超时（应快速退出并上抛 exit code）")
	}
	total := len(out) + len(errOut)
	if total > probeOutputCap+4096 {
		t.Fatalf("捕获未封顶: stdout=%d stderr=%d cap=%d", len(out), len(errOut), probeOutputCap)
	}
}

// TestTailBuffer_RetainsTail 满桶后保留尾部 cap 字节；Write 全量吞入
// （排水不封顶）；多次追加仍保留最新尾部。
func TestTailBuffer_RetainsTail(t *testing.T) {
	tb := tailBuffer{cap: 8}
	n, err := tb.Write([]byte("1234567890"))
	if err != nil || n != 10 {
		t.Fatalf("Write 必须全量吞入返回 len(p),nil（排水不封顶）: n=%d err=%v", n, err)
	}
	if got := tb.String(); got != "34567890" {
		t.Fatalf("tailBuffer 应保留尾部 8 字节, got %q", got)
	}
	if _, err := tb.Write([]byte("ABC")); err != nil {
		t.Fatal(err)
	}
	if got := tb.String(); got != "67890ABC" {
		t.Fatalf("追加写后应仍保留最新尾部 8 字节, got %q", got)
	}
}

// TestProbePaths ffprobe 路径推导共用一份（video/audio 同口径）：
// 绝对路径同目录推导 / 缺省按名 / 裸名不误推导。
func TestProbePaths(t *testing.T) {
	// 完整路径："<前缀>ffmpeg" ⇒ 同目录 "<前缀>ffprobe"
	f, p := probePaths(&Config{FFmpegPath: "/opt/ffmpeg/bin/ffmpeg"})
	if f != "/opt/ffmpeg/bin/ffmpeg" || p != "/opt/ffmpeg/bin/ffprobe" {
		t.Fatalf("绝对路径推导错: ffmpeg=%q ffprobe=%q", f, p)
	}
	// 缺省：FFmpegPath 空 ⇒ 按名找 PATH
	f2, p2 := probePaths(&Config{})
	if f2 != "ffmpeg" || p2 != "ffprobe" {
		t.Fatalf("缺省推导错: ffmpeg=%q ffprobe=%q", f2, p2)
	}
	// 裸名（无目录前缀，LastIndex==0 不推导）⇒ 两者都按名
	f3, p3 := probePaths(&Config{FFmpegPath: "ffmpeg"})
	if f3 != "ffmpeg" || p3 != "ffprobe" {
		t.Fatalf("裸名推导错: ffmpeg=%q ffprobe=%q", f3, p3)
	}
}

// TestPreviewSpawnsGoThroughProbeCmd 机械守卫：video.go/audio.go 源码零处
// exec 直起子进程（含 exec.Command 与 exec.CommandContext，前者是后者的
// 前缀子串）——五处调用点全部收口 newProbeCmd（唯一环境门禁点），绕过
// = 凭据泄漏面回归即红；新增调用点忘走门禁同样红。
func TestPreviewSpawnsGoThroughProbeCmd(t *testing.T) {
	for _, f := range []string{"video.go", "audio.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读源码失败: %v", err)
		}
		if strings.Contains(string(src), "exec.Command") {
			t.Errorf("%s 出现 exec 直起子进程——必须经 newProbeCmd（唯一环境门禁点），绕过 = 凭据泄漏面回归", f)
		}
	}
}

// TestNewProbeCmd_TreeKillWired（core#71 收编锁）：newProbeCmd 必须经
// procx.Command（Setpgid 建组=组击杀收整树）——换回裸 exec.CommandContext 即红。
func TestNewProbeCmd_TreeKillWired(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Setpgid 为 unix SysProcAttr 字段")
	}
	cmd := newProbeCmd(context.Background(), "true")
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("newProbeCmd 未走 procx（Setpgid 未挂）——整树终止回归")
	}
}
