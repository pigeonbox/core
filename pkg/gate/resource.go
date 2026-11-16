package gate

// Resource Policy 收口（core#173 第六片）：**上传容量与后台任务预算的单一执行点**。
//
// #173 的 Resource 面 = 上传容量、并发、连接数、后台任务预算的统一执行点。本文件
// 收口其中的「判定」——容量/预算类判定必须经本文件的函数，handler/域层不得散落
// 裸判据（`utils.GetMaxFileSize(); limit > 0 && size > limit` 这类手写比较就是
// 被收口的对象）：
//
//	单文件上限   MaxFileSize / CheckFileSize / ExceedsFileSize  （upload.max_file_size）
//	单请求体上限 MaxRequestSize / ExceedsRequestSize             （upload.upload_size）
//	文本分享上限 TextShareMaxBytes / ExceedsTextShare            （upload.text_max_bytes）
//	请求体预算   HTTPBodyLimit                                   （max(upload_size, max_file_size)）
//	总量预检     CheckUploadSizeCap（gate.go，#196 前已收口）
//	匿名日配额   CheckAnonymousQuota（quota.go，治理 2026-10-03 已收口）
//	配额账本     app/share/quota_reserve.reserveUploadCaps（#196 原子预留，域内单点）
//	后台任务预算 MinTaskInterval / CheckTaskBudget
//
// 文案口径逐字保留（行为零变化）：
//   - 标准形态 CheckFileSize 复用 utils.FileTooLargeError ⇒ 10010 / HTTP 413 /
//     「文件过大: 最大允许 N 字节」，与迁移前 utils.CheckWholeFileSize 调用点同文案；
//   - 阈值形态 Exceeds* 返回 (是否超限, 生效上限)，由调用方拼**自己的**文案
//     （「合并后文件大小 %d 超过上限 %d」等各通道既有措辞），判定仍在本执行点；
//   - 0 = 不限（三个上限同口径）；匿名豁免走 CheckUploadSizeCap（userID nil 跳过）
//     与 CheckAnonymousQuota，均不在本文件重复实现。
//
// 指标口径：CheckFileSize 超限记 metrics.RejectSize（与被它取代的
// utils.CheckUploadSize 一致）；Exceeds* 不记指标——那 4 个调用点迁移前就不记
// （handler 自有 JSON 出口），保持行为零变化。
//
// 守卫：bootstrap/resource_entry_guard_test.go（登记 × 路由矩阵 × 源码标记 +
// 四维负对照）；执行点自身行为由 pkg/gate/resource_test.go 钉住。
import (
	"fmt"
	"time"

	"github.com/pigeonbox/core/pkg/metrics"
	"github.com/pigeonbox/core/pkg/utils"
)

// ===== 单文件上限（upload.max_file_size）=====

// MaxFileSize 生效的整文件大小上限（字节；0 = 不限）。
// 配置未初始化（测试/启动早期）返回 0 = 不限，与迁移前 utils.GetMaxFileSize 同。
func MaxFileSize() int64 { return utils.GetMaxFileSize() }

// CheckFileSize 整文件上限判定（标准形态）：超限记 RejectSize 指标并返回
// typed error（ErrCode=10010 ⇒ HTTP 413，文案「文件过大: 最大允许 N 字节」）；
// 0 = 不限恒通过；负数大小恒通过（与 utils.CheckUploadSize 的 <=0 放行一致，
// 「文件大小必须大于0」的入参校验由各调用点自行负责）。
func CheckFileSize(size int64) error {
	limit := MaxFileSize()
	if limit > 0 && size > limit {
		metrics.RecordRejected(metrics.RejectSize)
		return utils.NewFileTooLargeError(limit)
	}
	return nil
}

// ExceedsFileSize 整文件上限判定（阈值形态）：返回 (是否超限, 生效上限)。
// 供**必须保留自有文案**的调用点使用（合并后实际大小复查、multipart 逐文件
// 拒绝、联邦转发逐文件拒绝）——判定收口在本执行点，措辞留在调用方。
// 不记指标，见文件头「指标口径」。
func ExceedsFileSize(size int64) (exceeded bool, limit int64) {
	limit = MaxFileSize()
	return limit > 0 && size > limit, limit
}

// ===== 单请求体上限（upload.upload_size）=====

// MaxRequestSize 生效的单请求体上限（字节；0 = 不限）。
// 与整文件上限是**两个旋钮**：分片/直传通道的存在意义即突破单请求限制，
// 整文件一律走 MaxFileSize（见 app/chunk/service.go 的回归注释）。
func MaxRequestSize() int64 { return utils.GetMaxUploadSize() }

// ExceedsRequestSize 单请求体上限判定（阈值形态，调用方自有文案）。
func ExceedsRequestSize(size int64) (exceeded bool, limit int64) {
	limit = MaxRequestSize()
	return limit > 0 && size > limit, limit
}

// ===== 文本分享上限（upload.text_max_bytes）=====

// TextShareMaxBytes 生效的文本分享上限（字节）；未配置(<=0)回退默认 222KB
// （utils.DefaultTextShareMaxBytes，对齐上游）。
func TextShareMaxBytes() int64 { return utils.GetTextShareMaxBytes() }

// ExceedsTextShare 文本分享上限判定（阈值形态，调用方自有文案）。
func ExceedsTextShare(size int64) (exceeded bool, limit int64) {
	limit = TextShareMaxBytes()
	return limit > 0 && size > limit, limit
}

// ===== 应用层请求体预算 =====

// defaultHTTPBodyLimit 未配置 upload.upload_size 时的应用层请求体上限（10MB，
// 与迁移前 bootstrap 起服路径同值）。
const defaultHTTPBodyLimit int64 = 10 * 1024 * 1024

// HTTPBodyLimit 应用层请求体上限（字节）：max(upload.upload_size, upload.max_file_size)，
// upload_size 未配置回退 10MB。
//
// 为什么取较大值：presign 中转（upload-direct）按流落盘，大文件不整体进内存——
// 只按 upload_size 设限会把 local/webdav 下 ≥100MB 的直传链路整条拦成 413
// （历史修复见 bootstrap 起服处注释）。
//
// 不变量（AGENTS 硬规则，对端是 nginx 的 PB_MAX_BODY_SIZE）：
//   - nginx PB_MAX_BODY_SIZE ≥ 服务端这个值（物理防线 ≥ 业务上限）；
//   - 服务端 max_file_size ≤ 本值（单文件永远发得出去）；
//   - user.user_upload_size（总量，CheckUploadSizeCap）≥ max_file_size。
func HTTPBodyLimit() int64 {
	// 走执行点自身 getter（utils 同源：nil ⇒ 0），不直读 conf——
	// internal/arch 规则8 棘轮：容量面的全局配置读取只准落在 utils/gate 工具层。
	uploadSize := MaxRequestSize()
	if uploadSize <= 0 {
		uploadSize = defaultHTTPBodyLimit
	}
	if maxFile := MaxFileSize(); maxFile > uploadSize {
		uploadSize = maxFile
	}
	return uploadSize
}

// ===== 容量不变量 =====

// BatchCapBelowFileCap 「单次发送总量上限 < 单文件上限」的**反不变量**判定
// （AGENTS 硬规则：总量 ≥ 单文件，否则超大文件永远发不出去——先被总量闸拒，
// UI 却仍在宣称单文件上限有效；2026-10-30 出厂默认自相矛盾事故即此类）。
//
// 任一为 0（= 不限）不构成矛盾，与管理台告警/对齐按钮、前端 uploadCapMismatch
// 同口径。判定收口在本执行点，错误文案归调用方（app/config 保存期校验）。
func BatchCapBelowFileCap(total, perFile int64) bool {
	return total > 0 && perFile > 0 && total < perFile
}

// ===== 后台任务预算 =====

// MinTaskInterval 后台周期任务的周期地板（1 分钟）。
//
// 防的是「配置一改把 janitor/GC 轮询调成毫秒级」这类把 DB 打爆的热循环；
// 与 app/federation 的 minSyncInterval 同值——联邦邀请同步的周期下限也是 1 分钟，
// 两个地板同源。装配口 CheckTaskBudget 违例即拒绝起跑（错误日志 + 不启动）。
const MinTaskInterval = time.Minute

// CheckTaskBudget 后台周期任务预算判定（启动口的单一判据）：
//
//   - 任务名非空（可观测性：无名循环没法对账/告警）；
//   - 0 < MinTaskInterval ≤ interval（周期有界，防热循环）；
//   - 0 ≤ firstDelay ≤ interval（首跑延迟非负且不超过一个周期——超过即配置笔误，
//     意味着首跑被推迟到第二个周期之后）。
//
// 返回普通 error（起服期配置问题，不是面向请求的业务错误，无业务码）。
func CheckTaskBudget(name string, interval, firstDelay time.Duration) error {
	if name == "" {
		return fmt.Errorf("后台任务缺少名称（loop.Config.Name）")
	}
	if interval <= 0 {
		return fmt.Errorf("后台任务 %q 周期必须为正，实际 %v", name, interval)
	}
	if interval < MinTaskInterval {
		return fmt.Errorf("后台任务 %q 周期 %v 低于预算地板 %v", name, interval, MinTaskInterval)
	}
	if firstDelay < 0 {
		return fmt.Errorf("后台任务 %q 首跑延迟为负: %v", name, firstDelay)
	}
	if firstDelay > interval {
		return fmt.Errorf("后台任务 %q 首跑延迟 %v 超过周期 %v", name, firstDelay, interval)
	}
	return nil
}
