// Package preview 生成图片/视频/音频预览（缩略图、时长、分辨率）与文本代码高亮。
//
// # 子进程治理（core#84 + core#71 D2 收编落地）
//
// 视频/音频预览依赖外部 ffmpeg/ffprobe，经 preview.newProbeCmd 起子进程，
// 两条约束已机械落地：
//
//   - **环境 = deny-by-default allowlist（core#84 → 收编 oekit/pkg procx
//     ChildEnv，pkg v0.41.0 / pkg#314）**：一切 exec 统一经 newProbeCmd 注入
//     childEnv()，凭据变量不进子进程（denylist 永远漏掉新加的凭据变量；
//     allowlist 的漏项只表现为少一个无害变量，方向安全）。allowlist 表已
//     **整表收编上游**（表只该有一份：新增基础变量要改两处，漏一处就是
//     「子进程少一个变量」的随机故障）；本仓仅经 extra 显式补一项
//     USERDOMAIN。输出统一经 runProbeOutput 有界捕获（尾部 probeOutputCap
//     字节，排水不封顶 ⇒ 子进程不阻塞在 write）——此处**保留本仓实现**：上游
//     WithOutputLimit 是「截头留头」，而 ffmpeg/ffprobe 的报错在 stderr 末尾，
//     截头会把唯一有用的诊断丢掉（见 childenv.go 的取舍说明）。接入的
//     转码驱动不得假定自己能读到宿主全量环境。
//   - **整树终止收编 procx（core#71 D2 → oekit/pkg#284 → procx v0.38.3）**：
//     此前 exec.CommandContext 的 ctx 到期只杀直接子进程，包装器型驱动
//     守护化孙进程时即泄漏面。现 unix = Setpgid 建组 → 组 TERM → 宽限
//     SIGKILL + 跨组逃逸清扫；windows = Job Object 整树终止（无优雅终止档，
//     上游如实声明的语义降级）。D3 过渡约束（「驱动不得守护化」）退役。
//
// 关联：core#71（决策记录）、core#84（环境门禁）、oekit/pkg#284（上游原语）。
package preview
