package preview

import (
	"context"
	"os/exec"
	"strings"

	"github.com/oekit/pkg/procx"
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
// core#71 D2 落地：子进程整树终止已收编 oekit/pkg procx（v0.38.3 起）——
// newProbeCmd 全部走 procx.Command（Setpgid 建组 → 组 TERM → 宽限 SIGKILL
// + 跨组逃逸清扫；windows Job Object），doc.go 的 D3 过渡约束（「驱动不得
// 守护化」）随之退役。

// probeOutputCap 单个子进程 stdout/stderr 的捕获上限（保留尾部）。
// 超限截头留尾进诊断——恶意媒体的 stderr 刷屏不再能吃满内存。
const probeOutputCap = 64 << 10 // 64KiB

// childEnv 返回预览子进程的最小环境：收编 oekit/pkg procx.ChildEnv
// （pkg v0.41.0 起提供，pkg#314 补齐 pkg#294 承诺的 env 半边）。
//
// 为什么整表收编而不是本仓自维护白名单：
//   - allowlist 表**只该有一份**——本仓此前自维护的 previewEnvAllow 与上游
//     baseEnvAllow 是两张独立演化的表，新增基础变量（如 SSL_CERT_FILE、TERM、
//     USER）要改两处，漏一处就是「子进程少一个变量」的随机故障（ffmpeg 找不到
//     CA 锚 ⇒ TLS 握手失败这类「认证莫名失败」形态）；
//   - 上游表是**allowlist（默认拒绝）**口径，与本仓原口径一致（denylist 永远
//     漏掉新加的凭据变量，allowlist 的漏项只表现为少一个无害变量，方向安全）；
//   - 上游额外处理了 windows 环境变量名大小写不敏感（envAllowed 按 GOOS 归一），
//     本仓手写版没有这层。
//
// 上游 ChildEnv 会尾部追加 GIT_TERMINAL_PROMPT=0 与 CI=1（git 弹交互会挂到
// 超时；对 ffmpeg/ffprobe 无害，且属上游刻意的「禁交互」纪律）。
//
// extra 里显式补一项 USERDOMAIN：上游 baseEnvAllow 未收录它（本仓原白名单有）。
// 取舍依据——它是 windows **域信息**（非路径类），ffmpeg/ffprobe 解码不需要，
// 与 SystemRoot「缺失时大量程序直接失能」不同级；但它是本仓原有白名单项，
// 收编时不静默丢弃（上游 ChildEnv 的 extra 通道正是「消费方显式声明自己需要
// 什么」的设计：白名单是通用最小集，工具特定项走 extra 按名放行）。
func childEnv() []string {
	return procx.ChildEnv([]string{"USERDOMAIN"})
}

// tailBuffer 有界尾部缓冲：Write 全量吞入（返回 len(p), nil ⇒ 排水不封顶，
// 子进程永不阻塞在 write 上），内存只保留最后 cap 字节（超限截头留尾）。
// cap<=0 视为不封顶。
//
// ⚠️ **为什么没有换成 oekit/pkg procx.WithOutputLimit**（v0.41.0 已提供，
// pkg#314 补齐的输出封顶半边）：两者**截断方向相反**——
//   - 上游 WithOutputLimit：**截头留头**（cappedBuffer 超限后只保留开头 cap
//     字节，报 StdoutTruncated/StderrTruncated 标记）；
//   - 本仓 tailBuffer：**截头留尾**（超限后保留最后 cap 字节）。
//
// 本仓要的是尾部：ffmpeg/ffprobe 的**报错信息在 stderr 末尾**（文件头几行是
// 版本横幅/流信息，末尾才是 "Invalid data found" 这类真因），截头留头会把
// 唯一有用的诊断丢掉，只剩一段横幅。诊断面退化到「看不出为什么失败」是不可
// 接受的——这属**真会改变行为**的语义差异，故保留本仓实现（用户口径：
// 机械等价替换一律做掉，真语义差异保留并写明理由）。
//
// 上游封顶的两项能力本仓另行吃到：
//   - 排水语义（边读边截 + 继续排空 pipe ⇒ 子进程不阻塞在 write）：本仓
//     tailBuffer.Write 同样「全量吞入返回 len(p), nil」，与上游同口径；
//   - 截断可判定：上游有 StdoutTruncated/StderrTruncated 标记，本仓
//     tailBuffer 无该标记（截断发生但调用方无从得知）——这是上游领先本仓
//     的一点，见下条 issue。
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

// newProbeCmd 构造预览子进程命令——**唯一的子进程环境门禁点 + 整树终止**：
// cmd.Env = childEnv()（deny-by-default）+ procx.Command（ctx 到期整树收场，
// 替代 exec.CommandContext 的只杀直接子进程；core#71 D2 收编 oekit/pkg procx）。
// preview 包内一切 exec 都必须经它（TestPreviewSpawnsGoThroughProbeCmd 源码
// 守卫锁死；绕过 = 凭据泄漏面/进程残留回归）。
func newProbeCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := procx.Command(ctx, name, args...)
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
