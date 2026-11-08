package preview

import (
	"context"
	"os"
	"os/exec"
	"strings"
)

// 本文件是 core#84 的落地：预览子进程凭据门禁 + 输出有界捕获。
//
// 背景（core#84）：preview 包 5 处 exec.CommandContext 均未设 cmd.Env ⇒
// Go 语义是子进程继承父进程全部环境变量 ⇒ ffmpeg/ffprobe 这个纯解码器
// 子进程持有 PB_ADMIN_PASSWORD / PB_JWT_SECRET / PB_STORAGE_* / PB_P2P*
// 全套服务端凭据——ffmpeg 是 CVE 大户且输入完全由匿名访客控制
// （open_upload=true 时任何人可触发预览），/proc/<pid>/environ 同机对他
// 进程可读（NAS/共享主机/容器多进程形态尤其成立）。顺带
// CombinedOutput/Output 无上限 ⇒ 恶意/损坏媒体让 ffmpeg 往 stderr 狂刷，
// 单请求即可吃满内存。
//
// ⚠️ 等上游收编后替换：oekit/pkg#284（procx 进程树终止原语提案，本段约束随其落地删除）。
// 直接依赖 oagentkit/runtime/procx.ChildEnv 会新增跨 org 依赖边（与 #71
// 的 procx 决策同构），两个上游通道有结论前先在本包内实现，并注释留痕。

// probeOutputCap 单个子进程 stdout/stderr 的捕获上限（保留尾部）。
// 超限截头留尾进诊断——恶意媒体的 stderr 刷屏不再能吃满内存。
const probeOutputCap = 64 << 10 // 64KiB

// previewEnvAllow 预览子进程的环境 allowlist：只放行这些键，其余全部剔除。
//
// 为什么默认拒绝而不是默认继承（deny-by-default）：denylist 永远漏掉新加
// 的凭据变量；allowlist 的漏项只是子进程少一个非凭据变量，方向安全。
var previewEnvAllow = map[string]bool{
	// —— 进程定位与 Locale（ffmpeg/ffprobe/转码驱动定位与运行必需）——
	"PATH":   true, // 可执行文件查找：FFmpegPath 未配绝对路径时按名定位
	"LANG":   true, // 本地化：报错文案与数字/时间格式
	"LC_ALL": true, // 显式 Locale（覆盖 LANG，部分发行版只认它）
	"TZ":     true, // 时区：ffprobe 时间戳输出口径
	// —— 临时目录与用户目录（ffmpeg 转码会写临时文件，剔掉可能解码失败）——
	"HOME":   true, // 字体缓存/临时文件回退位置
	"TMPDIR": true, // POSIX 临时目录
	"TEMP":   true, // Windows 临时目录
	"TMP":    true, // 临时目录别名（部分工具只认它）
	// —— 出站代理（转码驱动拉远端流/资源时需要走部署方代理）——
	"HTTP_PROXY":  true, // 大写代理（部分库只读大写形态）
	"HTTPS_PROXY": true, // 同上
	"NO_PROXY":    true, // 代理豁免清单（内网直连）
	"http_proxy":  true, // 小写代理（curl/ffmpeg 约定）
	"https_proxy": true, // 同上
	"no_proxy":    true, // 同上
	// —— Windows 子进程必需组（跨平台部署面）——
	"SystemRoot":  true, // 系统目录：多数 Windows 程序初始化必需
	"SystemDrive": true, // 系统盘符
	"COMSPEC":     true, // shell 路径
	"PATHEXT":     true, // 可执行扩展名解析
	"USERPROFILE": true, // 用户目录（HOME 的 Windows 形态）
	"USERDOMAIN":  true, // 域信息（部分运行库初始化读取）
}

// childEnv 返回预览子进程的最小环境：从 os.Environ() 过滤 allowlist，
// 无 `=` 的畸形项一并剔除（deny-by-default）。
func childEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || !previewEnvAllow[k] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// tailBuffer 有界尾部缓冲：Write 全量吞入（返回 len(p), nil ⇒ 排水不封顶，
// 子进程永不阻塞在 write 上），内存只保留最后 cap 字节（超限截头留尾）。
// cap<=0 视为不封顶。
type tailBuffer struct {
	buf []byte
	cap int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if t.cap > 0 && len(t.buf) > t.cap {
		t.buf = t.buf[len(t.buf)-t.cap:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }

// newProbeCmd 构造预览子进程命令——**唯一的子进程环境门禁点**：
// cmd.Env = childEnv()。preview 包内一切 exec 都必须经它
// （TestPreviewSpawnsGoThroughProbeCmd 源码守卫锁死；绕过 = 凭据泄漏面回归）。
func newProbeCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = childEnv()
	return cmd
}

// runProbeOutput 运行探测/转码子进程：stdout/stderr 各挂一个 tailBuffer
// **有界捕获尾部**（排水不封顶 ⇒ 子进程不阻塞在 write；内存只留尾部
// probeOutputCap 字节）。stdout 是数据时（ffprobe 探测）取第一个返回值，
// stderr 只进诊断。
func runProbeOutput(ctx context.Context, name string, args ...string) (stdoutTail, stderrTail string, err error) {
	cmd := newProbeCmd(ctx, name, args...)
	stdout := &tailBuffer{cap: probeOutputCap}
	stderr := &tailBuffer{cap: probeOutputCap}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err = cmd.Run()
	return stdout.String(), stderr.String(), err
}

// probeCtx 调用方 ctx 无 deadline 时补 probeTimeout（恶意/损坏文件可挂死
// ffmpeg/ffprobe 解析 ⇒ worker 被无限期占用）；已有 deadline 原样透传，
// cancel 为空操作（调用方可无条件 defer）。
func probeCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, probeTimeout)
}

// probePaths 推导 ffmpeg/ffprobe 可执行路径（video/audio 两生成器共用一份；
// audio 此前硬编码按名找 PATH 不走 config ⇒ 配了 FFmpegPath 的部署音频
// 探测仍找 PATH 里的二进制，与 video 口径不一致）。FFmpegPath 空则按名
// 找 PATH；配置为 "<前缀>ffmpeg" 时同目录推导 "<前缀>ffprobe"。
func probePaths(cfg *Config) (ffmpegPath, ffprobePath string) {
	f := cfg.FFmpegPath
	if f == "" {
		f = "ffmpeg"
	}
	ffprobePath = "ffprobe"
	if idx := strings.LastIndex(f, "ffmpeg"); idx > 0 {
		ffprobePath = f[:idx] + "ffprobe"
	}
	return f, ffprobePath
}
